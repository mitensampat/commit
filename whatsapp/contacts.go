package whatsapp

import (
	"context"
	"log"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/msfoundry/commit/store"
)

// syncContactNames copies WhatsApp's address-book names into commit.db.
//
// WhatsApp shows the user the name from their phone's address book, but
// message metadata only carries the contact's self-chosen push name — so a
// chat the user knows as "Allish Jain" arrives as "Allish". Anything that
// takes a name from the user (@schedule, @find, search) has to match against
// both. Names are stored under the phone JID and the LID, since inbound
// messages on modern accounts are addressed by LID.
func (c *Client) syncContactNames(ctx context.Context) {
	c.mu.RLock()
	client := c.wa
	c.mu.RUnlock()
	if client == nil || client.Store == nil || client.Store.Contacts == nil {
		return
	}

	contacts, err := client.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		log.Printf("contact sync: %v", err)
		return
	}

	batch := make([]store.ContactNames, 0, len(contacts)*2)
	for jid, info := range contacts {
		names := store.ContactNames{
			JID:       jid.ToNonAD().String(),
			FullName:  info.FullName,
			FirstName: info.FirstName,
			PushName:  info.PushName,
		}
		if len(names.Names()) == 0 {
			continue
		}
		batch = append(batch, names)

		// Mirror the same names onto the contact's other identity so a
		// LID-addressed chat still resolves to the address-book name.
		if other, ok := c.otherIdentity(ctx, jid); ok {
			mirrored := names
			mirrored.JID = other.String()
			batch = append(batch, mirrored)
		}
	}

	if len(batch) == 0 {
		return
	}
	if err := c.db.SaveContactNames(batch); err != nil {
		log.Printf("contact sync: save: %v", err)
		return
	}
	log.Printf("contact sync: %d contacts (%d identities)", len(contacts), len(batch))
}

// otherIdentity maps a phone JID to its LID or vice versa.
func (c *Client) otherIdentity(ctx context.Context, jid types.JID) (types.JID, bool) {
	c.mu.RLock()
	client := c.wa
	c.mu.RUnlock()
	if client == nil || client.Store == nil || client.Store.LIDs == nil {
		return types.JID{}, false
	}
	var out types.JID
	var err error
	switch jid.Server {
	case types.DefaultUserServer:
		out, err = client.Store.LIDs.GetLIDForPN(ctx, jid)
	case types.HiddenUserServer:
		out, err = client.Store.LIDs.GetPNForLID(ctx, jid)
	default:
		return types.JID{}, false
	}
	if err != nil || out.IsEmpty() {
		return types.JID{}, false
	}
	return out.ToNonAD(), true
}

// contactSyncLoop keeps names fresh; the address book changes rarely, so this
// is deliberately infrequent.
func (c *Client) contactSyncLoop(ctx context.Context) {
	// Let the initial connection settle before the first sync.
	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
	}
	c.syncContactNames(ctx)

	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.syncContactNames(ctx)
		}
	}
}
