// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package whatsmeow implements a client for interacting with the WhatsApp web multidevice API.
package whatsmeow

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/gookit/color"
	"go.mau.fi/whatsmeow/bindings"
	"go.mau.fi/whatsmeow/util"
	"github.com/whatsapp/whatsapp-web"
)

type Client struct {
	whatsappClient *whatsapp.Client
}

func NewClient() (*Client, error) {
	client, err := whatsapp.NewClient()
	if err != nil {
		return nil, err
	}
	return &Client{whatsappClient: client}, nil
}

func (c *Client) SendMessage(to string, message string) error {
	jid := util.ParseJID(to) // Assicurati che questa funzione gestisca i JID dei canali
	if jid == nil {
		return fmt.Errorf("invalid JID")
	}

	msg := whatsapp.TextMessage{
		To:      jid,
		Message: message,
	}
	_, err := c.whatsappClient.SendMessage(context.Background(), msg)
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	return nil
}

func (c *Client) HandleIncomingMessages() {
	for {
		select {
		case msg := <-c.whatsappClient.Messages:
			go c.processMessage(msg)
		}
	}
}

func (c *Client) processMessage(msg whatsapp.Message) {
	if msg.IsGroup {
		fmt.Printf("Received a message in group %s: %s\n", msg.Chat.Name, msg.Content)
	} else {
		fmt.Printf("Received a message from %s: %s\n", msg.Sender, msg.Content)
	}
}

func (c *Client) JoinChannel(jid string) error {
	channelJID := util.ParseJID(jid) // Assicurati che questa funzione gestisca i JID dei canali
	if channelJID == nil {
		return fmt.Errorf("invalid channel JID")
	}

	err := c.whatsappClient.JoinGroup(context.Background(), channelJID)
	if err != nil {
		return fmt.Errorf("failed to join channel: %w", err)
	}
	return nil
}

func (c *Client) LeaveChannel(jid string) error {
	channelJID := util.ParseJID(jid) // Assicurati che questa funzione gestisca i JID dei canali
	if channelJID == nil {
		return fmt.Errorf("invalid channel JID")
	}

	err := c.whatsappClient.LeaveGroup(context.Background(), channelJID)
	if err != nil {
		return fmt.Errorf("failed to leave channel: %w", err)
	}
	return nil
}

func (c *Client) ListChannels() ([]string, error) {
	channels, err := c.whatsappClient.GetGroups(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to list channels: %w", err)
	}

	var channelJIDs []string
	for _, ch := range channels {
		channelJIDs = append(channelJIDs, ch.JID)
	}
	return channelJIDs, nil
}
