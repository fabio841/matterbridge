//go:build whatsappmulti
// +build whatsappmulti

package bwhatsapp

import (
	"fmt"
	"mime"
	"strings"

	"github.com/42wim/matterbridge/bridge/config"
	"github.com/42wim/matterbridge/bridge/helper"

	"go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// nolint:gocritic
func (b *Bwhatsapp) eventHandler(evt interface{}) {
	switch e := evt.(type) {
	case *events.Message:
		b.handleMessage(e)
	case *events.GroupInfo:
		b.handleGroupInfo(e)
	}
}

func (b *Bwhatsapp) handleGroupInfo(event *events.GroupInfo) {
	b.Log.Debugf("Receiving event %#v", event)

	switch {
	case event.Join != nil:
		b.handleUserJoin(event)
	case event.Leave != nil:
		b.handleUserLeave(event)
	case event.Topic != nil:
		b.handleTopicChange(event)
	}
}

func (b *Bwhatsapp) handleUserJoin(event *events.GroupInfo) {
	for _, joinedJid := range event.Join {
		senderName := b.getSenderNameFromJID(joinedJid)

		rmsg := config.Message{
			UserID:   joinedJid.String(),
			Username: senderName,
			Channel:  event.JID.String(),
			Account:  b.Account,
			Protocol: b.Protocol,
			Event:    config.EventJoinLeave,
			Text:     "joined chat",
		}

		b.mutex.RLock()
		avatarURL, exists := b.userAvatars[joinedJid.String()]
		b.mutex.RUnlock()
		if exists {
			rmsg.Avatar = avatarURL
		}

		b.Log.Debugf("<= Sending user join message from %s on %s to gateway", joinedJid, b.Account)
		b.PostTextMessage(&rmsg) // Usare la funzione PostTextMessage per inviare il messaggio
	}
}

func (b *Bwhatsapp) handleUserLeave(event *events.GroupInfo) {
	for _, leftJid := range event.Leave {
		senderName := b.getSenderNameFromJID(leftJid)

		rmsg := config.Message{
			UserID:   leftJid.String(),
			Username: senderName,
			Channel:  event.JID.String(),
			Account:  b.Account,
			Protocol: b.Protocol,
			Event:    config.EventJoinLeave,
			Text:     "left chat",
		}

		b.mutex.RLock()
		avatarURL, exists := b.userAvatars[leftJid.String()]
		b.mutex.RUnlock()
		if exists {
			rmsg.Avatar = avatarURL
		}

		b.Log.Debugf("<= Sending user leave message from %s on %s to gateway", leftJid, b.Account)
		b.PostTextMessage(&rmsg) // Usare la funzione PostTextMessage per inviare il messaggio
	}
}

func (b *Bwhatsapp) handleTopicChange(event *events.GroupInfo) {
	msg := event.Topic
	senderJid := msg.TopicSetBy
	senderName := b.getSenderNameFromJID(senderJid)

	text := msg.Topic
	if text == "" {
		text = "removed topic"
	}

	rmsg := config.Message{
		UserID:   senderJid.String(),
		Username: senderName,
		Channel:  event.JID.String(),
		Account:  b.Account,
		Protocol: b.Protocol,
		Event:    config.EventTopicChange,
		Text:     "Topic changed: " + text,
	}

	b.mutex.RLock()
	avatarURL, exists := b.userAvatars[senderJid.String()]
	b.mutex.RUnlock()
	if exists {
		rmsg.Avatar = avatarURL
	}

	b.Log.Debugf("<= Sending topic change message from %s on %s to gateway", senderJid, b.Account)
	b.PostTextMessage(&rmsg) // Usare la funzione PostTextMessage per inviare il messaggio
}

func (b *Bwhatsapp) handleMessage(message *events.Message) {
	msg := message.Message
	switch {
	case msg == nil, message.Info.IsFromMe, message.Info.Timestamp.Before(b.startedAt):
		return
	}

	b.Log.Debugf("Receiving message %#v", msg)

	switch {
	case msg.Conversation != nil || msg.ExtendedTextMessage != nil:
		b.handleTextMessage(message.Info, msg)
	case msg.VideoMessage != nil:
		b.handleVideoMessage(message)
	case msg.AudioMessage != nil:
		b.handleAudioMessage(message)
	case msg.DocumentMessage != nil:
		b.handleDocumentMessage(message)
	case msg.ImageMessage != nil:
		b.handleImageMessage(message)
	case msg.ProtocolMessage != nil && *msg.ProtocolMessage.Type == proto.ProtocolMessage_REVOKE:
		b.handleDelete(msg.ProtocolMessage)
	}
}

// nolint:funlen
func (b *Bwhatsapp) handleTextMessage(messageInfo types.MessageInfo, msg *proto.Message) {
	senderJID := messageInfo.Sender
	channel := messageInfo.Chat

	senderName := b.getSenderName(messageInfo)

	if msg.GetExtendedTextMessage() == nil && msg.GetConversation() == "" {
		b.Log.Debugf("message without text content? %#v", msg)
		return
	}

	var text string

	// nolint:nestif
	if msg.GetExtendedTextMessage() == nil {
		text = msg.GetConversation()
	} else if msg.GetExtendedTextMessage().GetContextInfo() == nil {
		// Handle pure text message with a link preview
		// A pure text message with a link preview acts as an extended text message but will not contain any context info
		text = msg.GetExtendedTextMessage().GetText()
	} else {
		text = msg.GetExtendedTextMessage().GetText()
		ci := msg.GetExtendedTextMessage().GetContextInfo()

		if senderJID == (types.JID{}) && ci.Participant != nil {
			senderJID = types.NewJID(ci.GetParticipant(), types.DefaultUserServer)
		}

		if ci.MentionedJID != nil {
			// handle user mentions
			for _, mentionedJID := range ci.MentionedJID {
				numberAndSuffix := strings.SplitN(mentionedJID, "@", 2)

				// mentions comes as telephone numbers and we don't want to expose it to other bridges
				// replace it with something more meaninful to others
				mention := b.getSenderNotify(types.NewJID(numberAndSuffix[0], types.DefaultUserServer))

				text = strings.Replace(text, "@"+numberAndSuffix[0], "@"+mention, 1)
			}
		}
	}

	parentID := ""
	if msg.GetExtendedTextMessage() != nil {
		ci := msg.GetExtendedTextMessage().GetContextInfo()
		parentID = getParentIdFromCtx(ci)
	}

	rmsg := config.Message{
		UserID:   senderJID.String(),
		Username: senderName,
		Text:     text,
		Channel:  channel.String(),
		Account:  b.Account,
		Protocol: b.Protocol,
		Extra:    make(map[string][]interface{}),
		ID:       getMessageIdFormat(senderJID, messageInfo.ID),
		ParentID: parentID,
	}

	b.mutex.RLock()
	if avatarURL, exists := b.userAvatars[senderJID.String()]; exists {
		rmsg.Avatar = avatarURL
	}
	b.mutex.RUnlock()

	b.Log.Debugf("<= Sending text message from %s on %s to gateway", senderJID, b.Account)
	b.PostTextMessage(&rmsg) // Usare la funzione PostTextMessage per inviare il messaggio
}

// HandleImageMessage sent from WhatsApp, relay it to the bridge
func (b *Bwhatsapp) handleImageMessage(msg *events.Message) {
	imsg := msg.Message.GetImageMessage()

	senderJID := msg.Info.Sender
	senderName := b.getSenderName(msg.Info)
	ci := imsg.GetContextInfo()

	if senderJID == (types.JID{}) && ci.Participant != nil {
		senderJID = types.NewJID(ci.GetParticipant(), types.DefaultUserServer)
	}

	rmsg := config.Message{
		UserID:   senderJID.String(),
		Username: senderName,
		Channel:  msg.Info.Chat.String(),
		Account:  b.Account,
		Protocol: b.Protocol,
		Extra:    make(map[string][]interface{}),
		ID:       getMessageIdFormat(senderJID, msg.Info.ID),
		ParentID: getParentIdFromCtx(ci),
	}

	b.mutex.RLock()
	if avatarURL, exists := b.userAvatars[senderJID.String()]; exists {
		rmsg.Avatar = avatarURL
	}
	b.mutex.RUnlock()

	// Download the image
	data, err := b.wc.Download(msg.Info.MessageSource, imsg)
	if err != nil {
		b.Log.Errorf("image download failed: %s", err)
		return
	}

	rmsg.Text = imsg.GetCaption()

	// Try to get the image extension from the file header
	ext := helper.GetFileExtension(data)
	if ext == ".bin" {
		// If not possible, try to get the extension from the mimetype
		exts, err := mime.ExtensionsByType(imsg.GetMimetype())
		if err == nil {
			ext = exts[0]
		}
	}

	comment := "image " + msg.Info.ID + ext
	helper.HandleDownloadData(b.Log, &rmsg, comment, imsg.GetMimetype(), data, b.General)
	b.Log.Debugf("<= Sending image message from %s on %s to gateway", senderJID, b.Account)
	b.PostImageMessage(&rmsg) // Usare la funzione PostImageMessage per inviare il messaggio
}

// HandleVideoMessage sent from WhatsApp, relay it to the bridge
func (b *Bwhatsapp) handleVideoMessage(msg *events.Message) {
	vmsg := msg.Message.GetVideoMessage()

	senderJID := msg.Info.Sender
	senderName := b.getSenderName(msg.Info)
	ci := vmsg.GetContextInfo()

	if senderJID == (types.JID{}) && ci.Participant != nil {
		senderJID = types.NewJID(ci.GetParticipant(), types.DefaultUserServer)
	}

	rmsg := config.Message{
		UserID:   senderJID.String(),
		Username: senderName,
		Channel:  msg.Info.Chat.String(),
		Account:  b.Account,
		Protocol: b.Protocol,
		Extra:    make(map[string][]interface{}),
		ID:       getMessageIdFormat(senderJID, msg.Info.ID),
		ParentID: getParentIdFromCtx(ci),
	}

	b.mutex.RLock()
	if avatarURL, exists := b.userAvatars[senderJID.String()]; exists {
		rmsg.Avatar = avatarURL
	}
	b.mutex.RUnlock()

	// Download the video
	data, err := b.wc.Download(msg.Info.MessageSource, vmsg)
	if err != nil {
		b.Log.Errorf("video download failed: %s", err)
		return
	}

	rmsg.Text = vmsg.GetCaption()

	// Try to get the video extension from the file header
	ext := helper.GetFileExtension(data)
	if ext == ".bin" {
		// If not possible, try to get the extension from the mimetype
		exts, err := mime.ExtensionsByType(vmsg.GetMimetype())
		if err == nil {
			ext = exts[0]
		}
	}

	comment := "video " + msg.Info.ID + ext
	helper.HandleDownloadData(b.Log, &rmsg, comment, vmsg.GetMimetype(), data, b.General)
	b.Log.Debugf("<= Sending video message from %s on %s to gateway", senderJID, b.Account)
	b.PostVideoMessage(&rmsg) // Usare la funzione PostVideoMessage per inviare il messaggio
}

// HandleDocumentMessage sent from WhatsApp, relay it to the bridge
func (b *Bwhatsapp) handleDocumentMessage(msg *events.Message) {
	dmsg := msg.Message.GetDocumentMessage()

	senderJID := msg.Info.Sender
	senderName := b.getSenderName(msg.Info)
	ci := dmsg.GetContextInfo()

	if senderJID == (types.JID{}) && ci.Participant != nil {
		senderJID = types.NewJID(ci.GetParticipant(), types.DefaultUserServer)
	}

	rmsg := config.Message{
		UserID:   senderJID.String(),
		Username: senderName,
		Channel:  msg.Info.Chat.String(),
		Account:  b.Account,
		Protocol: b.Protocol,
		Extra:    make(map[string][]interface{}),
		ID:       getMessageIdFormat(senderJID, msg.Info.ID),
		ParentID: getParentIdFromCtx(ci),
	}

	b.mutex.RLock()
	if avatarURL, exists := b.userAvatars[senderJID.String()]; exists {
		rmsg.Avatar = avatarURL
	}
	b.mutex.RUnlock()

	// Download the document
	data, err := b.wc.Download(msg.Info.MessageSource, dmsg)
	if err != nil {
		b.Log.Errorf("document download failed: %s", err)
		return
	}

	comment := "document " + dmsg.GetFileName()
	helper.HandleDownloadData(b.Log, &rmsg, comment, dmsg.GetMimetype(), data, b.General)
	b.Log.Debugf("<= Sending document message from %s on %s to gateway", senderJID, b.Account)
	b.PostDocumentMessage(&rmsg) // Usare la funzione PostDocumentMessage per inviare il messaggio
}

// HandleAudioMessage sent from WhatsApp, relay it to the bridge
func (b *Bwhatsapp) handleAudioMessage(msg *events.Message) {
	amsg := msg.Message.GetAudioMessage()

	senderJID := msg.Info.Sender
	senderName := b.getSenderName(msg.Info)
	ci := amsg.GetContextInfo()

	if senderJID == (types.JID{}) && ci.Participant != nil {
		senderJID = types.NewJID(ci.GetParticipant(), types.DefaultUserServer)
	}

	rmsg := config.Message{
		UserID:   senderJID.String(),
		Username: senderName,
		Channel:  msg.Info.Chat.String(),
		Account:  b.Account,
		Protocol: b.Protocol,
		Extra:    make(map[string][]interface{}),
		ID:       getMessageIdFormat(senderJID, msg.Info.ID),
		ParentID: getParentIdFromCtx(ci),
	}

	b.mutex.RLock()
	if avatarURL, exists := b.userAvatars[senderJID.String()]; exists {
		rmsg.Avatar = avatarURL
	}
	b.mutex.RUnlock()

	// Download the audio
	data, err := b.wc.Download(msg.Info.MessageSource, amsg)
	if err != nil {
		b.Log.Errorf("audio download failed: %s", err)
		return
	}

	comment := "voice message"
	helper.HandleDownloadData(b.Log, &rmsg, comment, amsg.GetMimetype(), data, b.General)
	b.Log.Debugf("<= Sending audio message from %s on %s to gateway", senderJID, b.Account)
	b.PostAudioMessage(&rmsg) // Usare la funzione PostAudioMessage per inviare il messaggio
}

// handleDelete handles message delete from WhatsApp, relays it to the bridge
func (b *Bwhatsapp) handleDelete(msg *proto.ProtocolMessage) {
	b.Log.Debugf("<= Sending delete message from %s on %s to gateway", b.Account, b.Protocol)
	b.Log.Debugf("Deleting message %s", msg.GetKey().GetId())

	b.Remote <- config.Message{
		Event: config.EventMsgDelete,
		ID:    msg.GetKey().GetId(),
	}
}

func getParentIdFromCtx(ci *proto.ContextInfo) string {
	if ci != nil && ci.QuotedMessageID != nil {
		return *ci.QuotedMessageID
	}
	return ""
}
