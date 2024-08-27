//go:build whatsappmulti
// +build whatsappmulti

package bwhatsapp

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sync"  // Importa sync per la sincronizzazione concorrente
	"time"

	"github.com/42wim/matterbridge/bridge"
	"github.com/42wim/matterbridge/bridge/config"
	"github.com/mdp/qrterminal"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	goproto "google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite" // necessaria per sqlite
)

const (
	// Parametri di configurazione dell'account
	cfgNumber = "Number"
)

// Struttura Bwhatsapp Bridge che mantiene tutte le informazioni necessarie per il bridging
type Bwhatsapp struct {
	*bridge.Config

	startedAt    time.Time
	wc           *whatsmeow.Client
	contacts     map[types.JID]types.ContactInfo
	users        map[string]types.ContactInfo
	userAvatars  map[string]string
	joinedGroups []*types.GroupInfo
	mutex        sync.RWMutex  // Aggiungi un mutex per la sincronizzazione concorrente
}

type Replyable struct {
	MessageID types.MessageID
	Sender    types.JID
}

// New Crea un nuovo bridge WhatsApp. Questo sarà chiamato per ogni voce [whatsapp.<server>] che hai nel file di configurazione
func New(cfg *bridge.Config) bridge.Bridger {
	number := cfg.GetString(cfgNumber)

	if number == "" {
		cfg.Log.Fatalf("Configurazione mancante per il bridge WhatsApp: Number")
	}

	b := &Bwhatsapp{
		Config: cfg,

		users:       make(map[string]types.ContactInfo),
		userAvatars: make(map[string]string),
		contacts:    make(map[types.JID]types.ContactInfo), // Inizializza la mappa dei contatti
	}

	return b
}

// Connect a WhatsApp. Implementazione richiesta dell'interfaccia Bridger
func (b *Bwhatsapp) Connect() error {
	device, err := b.getDevice()
	if err != nil {
		return err
	}

	number := b.GetString(cfgNumber)
	if number == "" {
		return errors.New("il numero di telefono di WhatsApp deve essere configurato")
	}

	b.Log.Debugln("Connessione a WhatsApp in corso..")

	b.wc = whatsmeow.NewClient(device, waLog.Stdout("Client", "INFO", true))
	b.wc.AddEventHandler(b.eventHandler)

	firstlogin := false
	var qrChan <-chan whatsmeow.QRChannelItem
	if b.wc.Store.ID == nil {
		firstlogin = true
		qrChan, err = b.wc.GetQRChannel(context.Background())
		if err != nil && !errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
			return errors.New("impossibile ottenere il canale QR:" + err.Error())
		}
	}

	err = b.wc.Connect()
	if err != nil {
		return errors.New("impossibile connettersi a WhatsApp: " + err.Error())
	}

	if b.wc.Store.ID == nil {
		for evt := range qrChan {
			if evt.Event == "code" {
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else {
				b.Log.Infof("Risultato del canale QR: %s", evt.Event)
			}
		}
	}

	// disconnetti e riconnetti al nostro primo accesso/accoppiamento
	// per qualche motivo la GetJoinedGroups in JoinChannel non funziona al primo accesso
	if firstlogin {
		b.wc.Disconnect()
		time.Sleep(time.Second)

		err = b.wc.Connect()
		if err != nil {
			return errors.New("impossibile connettersi a WhatsApp: " + err.Error())
		}
	}

	b.Log.Infoln("Connessione a WhatsApp avvenuta con successo")

	// Blocca il mutex prima di accedere ai contatti
	b.mutex.Lock()
	b.contacts, err = b.wc.Store.Contacts.GetAllContacts()
	b.mutex.Unlock()

	if err != nil {
		return errors.New("impossibile ottenere i contatti: " + err.Error())
	}

	b.joinedGroups, err = b.wc.GetJoinedGroups()
	if err != nil {
		return errors.New("impossibile ottenere l'elenco dei gruppi a cui si è uniti: " + err.Error())
	}

	b.startedAt = time.Now()

	// Mappa tutti gli utenti
	b.mutex.RLock()
	for id, contact := range b.contacts {
		if !isGroupJid(id.String()) && id.String() != "status@broadcast" {
			// è un utente
			b.users[id.String()] = contact
		}
	}
	b.mutex.RUnlock()

	// Ottieni avatar utente in modo asincrono
	b.Log.Info("Recupero degli avatar degli utenti in corso..")

	for jid := range b.users {
		go func(jid string) {  // Esegui il caricamento degli avatar in modo concorrente
			info, err := b.GetProfilePicThumb(jid)
			if err != nil {
				b.Log.Warnf("Impossibile ottenere la foto del profilo di %s: %v", jid, err)
			} else {
				b.mutex.Lock()
				if info != nil {
					b.userAvatars[jid] = info.URL
				}
				b.mutex.Unlock()
			}
		}(jid)
	}

	b.Log.Info("Completato il recupero degli avatar..")

	return nil
}

// Disconnect viene chiamato durante la riconnessione al bridge
// Implementazione richiesta dell'interfaccia Bridger
func (b *Bwhatsapp) Disconnect() error {
	b.wc.Disconnect()

	return nil
}

// JoinChannel Unisciti a un gruppo WhatsApp specificato nella configurazione del gateway come channel='number-id@g.us' o channel='Channel name'
// Implementazione richiesta dell'interfaccia Bridger
// https://github.com/42wim/matterbridge/blob/2cfd880cdb0df29771bf8f31df8d990ab897889d/bridge/bridge.go#L11-L16
func (b *Bwhatsapp) JoinChannel(channel config.ChannelInfo) error {
	byJid := isGroupJid(channel.Name)

	// verifica se siamo membri del gruppo specificato
	if byJid {
		gJID, err := types.ParseJID(channel.Name)
		if err != nil {
			return err
		}

		b.mutex.RLock()
		defer b.mutex.RUnlock()
		for _, group := range b.joinedGroups {
			if group.JID == gJID {
				return nil
			}
		}
	}

	foundGroups := []string{}

	b.mutex.RLock()
	defer b.mutex.RUnlock()
	for _, group := range b.joinedGroups {
		if group.Name == channel.Name {
			foundGroups = append(foundGroups, group.Name)
		}
	}

	switch len(foundGroups) {
	case 0:
		// nessun gruppo trovato - stampa le possibilità
		for _, group := range b.joinedGroups {
			b.Log.Infof("%s %s", group.JID, group.Name)
		}
		return fmt.Errorf("specifica l'ID del gruppo dall'elenco sopra invece del nome '%s'", channel.Name)
	case 1:
		return fmt.Errorf("il nome del gruppo potrebbe cambiare. Configura il gateway con channel=\"%v\" invece di channel=\"%v\"", foundGroups[0], channel.Name)
	default:
		return fmt.Errorf("ci sono più di un gruppo con il nome '%s'. Specifica uno degli ID come nome del canale: %v", channel.Name, foundGroups)
	}
}

// PostDocumentMessage posta un messaggio documento dal bridge a WhatsApp
func (b *Bwhatsapp) PostDocumentMessage(msg *config.Message, file *os.File) (string, error) {
	_, fileStr := filepath.Split(msg.Extra["file"][0].(config.FileInfo).URL)
	contentType := mime.TypeByExtension(filepath.Ext(fileStr))

	uploaded, err := b.wc.Upload(context.Background(), file, whatsmeow.MediaDocument)
	if err != nil {
		return "", errors.New("impossibile caricare il file: " + err.Error())
	}

	info := &proto.DocumentMessage{
		Url:           &uploaded.URL,
		Mimetype:      &contentType,
		FileName:      &fileStr,
		FileLength:    &uploaded.Size,
		MediaKey:      uploaded.MediaKey,
		DirectPath:    &uploaded.DirectPath,
		FileSha256:    uploaded.FileSHA256,
		FileEncSha256: uploaded.FileEncSHA256,
	}

	return b.sendMessage(msg, info)
}

// PostImageMessage posta un messaggio immagine dal bridge a WhatsApp
func (b *Bwhatsapp) PostImageMessage(msg *config.Message, file *os.File) (string, error) {
	uploaded, err := b.wc.Upload(context.Background(), file, whatsmeow.MediaImage)
	if err != nil {
		return "", errors.New("impossibile caricare l'immagine: " + err.Error())
	}

	info := &proto.ImageMessage{
		Url:           &uploaded.URL,
		MediaKey:      uploaded.MediaKey,
		Mimetype:      goproto.String("image/jpeg"),
		FileSha256:    uploaded.FileSHA256,
		FileEncSha256: uploaded.FileEncSHA256,
		DirectPath:    &uploaded.DirectPath,
	}

	return b.sendMessage(msg, info)
}

// PostVideoMessage posta un messaggio video dal bridge a WhatsApp
func (b *Bwhatsapp) PostVideoMessage(msg *config.Message, file *os.File) (string, error) {
	uploaded, err := b.wc.Upload(context.Background(), file, whatsmeow.MediaVideo)
	if err != nil {
		return "", errors.New("impossibile caricare il video: " + err.Error())
	}

	info := &proto.VideoMessage{
		Url:           &uploaded.URL,
		MediaKey:      uploaded.MediaKey,
		Mimetype:      goproto.String("video/mp4"),
		FileSha256:    uploaded.FileSHA256,
		FileEncSha256: uploaded.FileEncSHA256,
		DirectPath:    &uploaded.DirectPath,
	}

	return b.sendMessage(msg, info)
}

// PostAudioMessage posta un messaggio audio dal bridge a WhatsApp
func (b *Bwhatsapp) PostAudioMessage(msg *config.Message, file *os.File) (string, error) {
	uploaded, err := b.wc.Upload(context.Background(), file, whatsmeow.MediaAudio)
	if err != nil {
		return "", errors.New("impossibile caricare l'audio: " + err.Error())
	}

	info := &proto.AudioMessage{
		Url:           &uploaded.URL,
		MediaKey:      uploaded.MediaKey,
		Mimetype:      goproto.String("audio/ogg; codecs=opus"),
		FileSha256:    uploaded.FileSHA256,
		FileEncSha256: uploaded.FileEncSHA256,
		DirectPath:    &uploaded.DirectPath,
	}

	return b.sendMessage(msg, info)
}

// PostTextMessage invia un messaggio di testo da Matterbridge a WhatsApp
func (b *Bwhatsapp) PostTextMessage(msg *config.Message) (string, error) {
	b.Log.Debugf("=> PostTextMessage: %#v", msg)

	// Identifica la modalità di invio del messaggio (reply a messaggio o messaggio semplice)
	var info interface{}
	if msg.ParentID != "" {
		// Modalità reply
		info = &proto.Message{
			Conversation: goproto.String(msg.Text),
			ContextInfo: &proto.ContextInfo{
				QuotedMessageID: &msg.ParentID,
				Participant:     goproto.String(msg.Username),
			},
		}
	} else {
		// Modalità messaggio semplice
		info = &proto.Message{
			Conversation: goproto.String(msg.Text),
		}
	}

	return b.sendMessage(msg, info)
}

// sendMessage invia un messaggio generico a WhatsApp
func (b *Bwhatsapp) sendMessage(msg *config.Message, content interface{}) (string, error) {
	// Controlla se il destinatario è una stanza
	jid, ok := parseJID(msg.Channel)
	if !ok {
		return "", errors.New("jid del destinatario non valido: " + msg.Channel)
	}

	_, err := b.wc.SendMessage(context.Background(), jid, content)
	if err != nil {
		return "", errors.New("impossibile inviare il messaggio: " + err.Error())
	}

	return "", nil
}
