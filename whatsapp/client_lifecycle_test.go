package whatsapp

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

func TestConfigureClientDelegatesReconnectsToWhatsmeow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := &Client{appCtx: ctx}
	wa := &whatsmeow.Client{}
	c.configureClient(wa)

	if !wa.EnableAutoReconnect {
		t.Fatal("whatsmeow auto-reconnect is disabled")
	}
	if wa.BackgroundEventCtx != ctx {
		t.Fatal("whatsmeow reconnect lifecycle is not bound to the application context")
	}
}

func TestConnectionEventsOnlyUpdateApplicationState(t *testing.T) {
	c := &Client{}

	c.handleEvent(&events.Connected{})
	if !c.IsConnected() {
		t.Fatal("Connected event did not mark the client connected")
	}

	c.handleEvent(&events.Disconnected{})
	if c.IsConnected() {
		t.Fatal("Disconnected event did not mark the client disconnected")
	}
}
