// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package whatsmeow implements a client for interacting with the WhatsApp web multidevice API.
package whatsmeow

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"go.mau.fi/util/random"
	"golang.org/x/net/proxy"

	"go.mau.fi/whatsmeow/appstate"
	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/util/keys"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// EventHandler is a function that can handle events from WhatsApp.
type EventHandler func(evt interface{})
type nodeHandler func(node *waBinary.Node)

var nextHandlerID uint32

type wrappedEventHandler struct {
	fn EventHandler
	id uint32
}

type deviceCache struct {
	devices []types.JID
	dhash   string
}

// Client contains everything necessary to connect to and interact with the WhatsApp web API.
type Client struct {
	Store   *store.Device
	Log     waLog.Logger
	recvLog waLog.Logger
	sendLog waLog.Logger

	socket     *socket.NoiseSocket
	socketLock sync.RWMutex
	socketWait chan struct{}

	isLoggedIn            atomic.Bool
	expectedDisconnect    atomic.Bool
	EnableAutoReconnect   bool
	LastSuccessfulConnect time.Time
	AutoReconnectErrors   int
	// AutoReconnectHook is called when auto-reconnection fails. If the function returns false,
	// the client will not attempt to reconnect. The number of retries can be read from AutoReconnectErrors.
	AutoReconnectHook func(error) bool

	sendActiveReceipts atomic.Uint32

	// EmitAppStateEventsOnFullSync can be set to true if you want to get app state events emitted
	// even when re-syncing the whole state.
	EmitAppStateEventsOnFullSync bool

	AutomaticMessageRerequestFromPhone bool
	pendingPhoneRerequests             map[types.MessageID]context.CancelFunc
	pendingPhoneRerequestsLock         sync.RWMutex

	appStateProc     *appstate.Processor
	appStateSyncLock sync.Mutex

	historySyncNotifications  chan *waProto.HistorySyncNotification
	historySyncHandlerStarted atomic.Bool

	uploadPreKeysLock sync.Mutex
	lastPreKeyUpload  time.Time

	mediaConnCache *MediaConn
	mediaConnLock  sync.Mutex

	responseWaiters     map[string]chan<- *waBinary.Node
	responseWaitersLock sync.Mutex

	nodeHandlers      map[string]nodeHandler
	handlerQueue      chan *waBinary.Node
	eventHandlers     []wrappedEventHandler
	eventHandlersLock sync.RWMutex

	messageRetries     map[string]int
	messageRetriesLock sync.Mutex

	incomingRetryRequestCounter     map[incomingRetryKey]int
	incomingRetryRequestCounterLock sync.Mutex

	appStateKeyRequests     map[string]time.Time
	appStateKeyRequestsLock sync.RWMutex

	messageSendLock sync.Mutex

	privacySettingsCache atomic.Value

	groupParticipantsCache     map[types.JID][]types.JID
	groupParticipantsCacheLock sync.Mutex
	userDevicesCache           map[types.JID]deviceCache
	userDevicesCacheLock       sync.Mutex

	recentMessagesMap  map[recentMessageKey]RecentMessage
	recentMessagesList [recentMessagesSize]recentMessageKey
	recentMessagesPtr  int
	recentMessagesLock sync.RWMutex

	sessionRecreateHistory     map[types.JID]time.Time
	sessionRecreateHistoryLock sync.Mutex
	// GetMessageForRetry is used to find the source message for handling retry receipts
	// when the message is not found in the recently sent message cache.
	GetMessageForRetry func(requester, to types.JID, id types.MessageID) *waProto.Message
	// PreRetryCallback is called before a retry receipt is accepted.
	// If it returns false, the accepting will be cancelled and the retry receipt will be ignored.
	PreRetryCallback func(receipt *events.Receipt, id types.MessageID, retryCount int, msg *waProto.Message) bool

	// PrePairCallback is called before pairing is completed. If it returns false, the pairing will be cancelled and
	// the client will disconnect.
	PrePairCallback func(jid types.JID, platform, businessName string) bool

	// GetClientPayload is called to get the client payload for connecting to the server.
	// This should NOT be used for WhatsApp (to change the OS name, update fields in store.BaseClientPayload directly).
	GetClientPayload func() *waProto.ClientPayload

	// Should untrusted identity errors be handled automatically? If true, the stored identity and existing signal
	// sessions will be removed on untrusted identity errors, and an events.IdentityChange will be dispatched.
	// If false, decrypting a message from untrusted devices will fail.
	AutoTrustIdentity bool

	// Should sending to own devices be skipped when sending broadcasts?
	// This works around a bug in the WhatsApp android app where it crashes if you send a status message from a linked device.
	DontSendSelfBroadcast bool

	// Should SubscribePresence return an error if no privacy token is stored for the user?
	ErrorOnSubscribePresenceWithoutToken bool

	phoneLinkingCache *phoneLinkingCache

	uniqueID  string
	idCounter atomic.Uint64

	proxy          Proxy
	socksProxy     proxy.Dialer
	proxyOnlyLogin bool
	http           *http.Client

	// This field changes the client to act like a Messenger client instead of a WhatsApp one.
	//
	// Note that you cannot use a Messenger account just by setting this field, you must use a
	// separate library for all the non-e2ee-related stuff like logging in.
	// The library is currently embedded in mautrix-meta (https://github.com/mautrix/meta), but may be separated later.
	MessengerConfig *MessengerConfig
	RefreshCAT      func() error

	// Channels to manage JID subscriptions
	channelJIDsLock sync.RWMutex
	channelJIDs     map[types.JID]bool
}

type MessengerConfig struct {
	UserAgent string
	BaseURL   string
}

// Size of buffer for the channel that all incoming XML nodes go through.
// In general it shouldn't go past a few buffered messages, but the channel is big to be safe.
const handlerQueueSize = 2048

// NewClient initializes a new WhatsApp web client.
//
// The logger can be nil, it will default to a no-op logger.
//
// The device store must be set. A default SQL-backed implementation is available in the store/sqlstore package.
//
//	container, err := sqlstore.New("sqlite3", "file:yoursqlitefile.db?_foreign_keys=on", nil)
//	if err != nil {
//		panic(err)
//	}
//	// If you want multiple sessions, remember their JIDs and use .GetDevice(jid) or .GetAllDevices() instead.
//	deviceStore, err := container.GetFirstDevice()
//	if err != nil {
//		panic(err)
//	}
//	client := whatsmeow.NewClient(deviceStore, nil)
func NewClient(deviceStore *store.Device, log waLog.Logger) *Client {
	if log == nil {
		log = waLog.Noop
	}
	uniqueIDPrefix := random.Bytes(2)
	cli := &Client{
		http: &http.Client{
			Transport: (http.DefaultTransport.(*http.Transport)).Clone(),
		},
		proxy:           http.ProxyFromEnvironment,
		Store:           deviceStore,
		Log:             log,
		recvLog:         log.Sub("Recv"),
		sendLog:         log.Sub("Send"),
		uniqueID:        fmt.Sprintf("%d.%d-", uniqueIDPrefix[0], uniqueIDPrefix[1]),
		responseWaiters: make(map[string]chan<- *waBinary.Node),
		eventHandlers:   make([]wrappedEventHandler, 0, 1),
		messageRetries:  make(map[string]int),
		handlerQueue:    make(chan *waBinary.Node, handlerQueueSize),
		appStateProc:    appstate.NewProcessor(deviceStore, log.Sub("AppState")),
		socketWait:      make(chan struct{}),

		incomingRetryRequestCounter: make(map[incomingRetryKey]int),

		historySyncNotifications: make(chan *waProto.HistorySyncNotification, 32),

		groupParticipantsCache: make(map[types.JID][]types.JID),
		userDevicesCache:       make(map[types.JID]deviceCache),

		recentMessagesMap:      make(map[recentMessageKey]RecentMessage, recentMessagesSize),
		sessionRecreateHistory: make(map[types.JID]time.Time),
		channelJIDs:            make(map[types.JID]bool),
	}
	cli.messageRetriesLock.Lock()
	cli.recentMessagesList[cli.recentMessagesPtr] = recentMessageKey{}
	cli.messageRetriesLock.Unlock()
	return cli
}

// AddJIDChannel associates a JID with a channel.
func (cli *Client) AddJIDChannel(jid types.JID, channel bool) {
	cli.channelJIDsLock.Lock()
	defer cli.channelJIDsLock.Unlock()
	cli.channelJIDs[jid] = channel
}

// RemoveJIDChannel removes a JID from the channel map.
func (cli *Client) RemoveJIDChannel(jid types.JID) {
	cli.channelJIDsLock.Lock()
	defer cli.channelJIDsLock.Unlock()
	delete(cli.channelJIDs, jid)
}

// GetJIDChannel retrieves the channel status of a JID.
func (cli *Client) GetJIDChannel(jid types.JID) (bool, bool) {
	cli.channelJIDsLock.RLock()
	defer cli.channelJIDsLock.RUnlock()
	channel, ok := cli.channelJIDs[jid]
	return channel, ok
}
