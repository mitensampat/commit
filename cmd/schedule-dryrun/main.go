// schedule-dryrun walks full @schedule sessions end to end against a COPY of
// the Commit DB, with a FAKE calendar and a FAKE sender: no real WhatsApp
// messages, no real calendar events. The LLM interpreter is real (key read
// from the DB copy), so this exercises the exact production pipeline:
//
//	command parse → contact resolve → context inference → slot computation →
//	draft → propose → watcher → interpretation → consent → re-verify → book
//
// Usage:
//
//	go run ./cmd/schedule-dryrun -db /path/to/db-copy-dir
//
// The directory must contain commit.db and .crypto_key COPIED from ~/.commit.
// The tool writes only to the copy (synthetic contact + messages).
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msfoundry/commit/calendar"
	"github.com/msfoundry/commit/schedule"
	"github.com/msfoundry/commit/store"

	_ "modernc.org/sqlite"
)

const selfJID = "919900000000@s.whatsapp.net"

// Each scenario gets its own contact so threads never bleed between
// scenarios (they all run within the same real-time minute).
var (
	contactJID  = "14155550123@s.whatsapp.net" // +1 → TZ inference kicks in
	contactName = "Dryrun Dave"
)

var allContactJIDs = []string{
	"14155550123@s.whatsapp.net",
	"14155550124@s.whatsapp.net",
	"14155550125@s.whatsapp.net",
	"14155550126@s.whatsapp.net",
}

// beginScenario points the helpers at a fresh contact and seeds its history.
func beginScenario(db *store.DB, idx int, name string) {
	contactJID = allContactJIDs[idx]
	contactName = name
	base := time.Now().Add(-24 * time.Hour)
	insertMsg(db, contactJID, "hey! good chatting at the offsite. we should sync on the partnership pilot", false, base)
	insertMsg(db, contactJID, "yes! let's find time this week, a quick call works", true, base.Add(5*time.Minute))
}

// fakeCalendar implements schedule.CalendarService over synthetic busy blocks.
type fakeCalendar struct {
	busy []calendar.Interval
	loc  *time.Location
}

func (f *fakeCalendar) Connected() bool { return true }

func (f *fakeCalendar) ComputeSlots(ctx context.Context, from, to time.Time, dur time.Duration, inPerson bool, days []time.Weekday) ([]schedule.Slot, error) {
	prefs := calendar.Prefs{
		DayStartMin: 9 * 60, DayEndMin: 18 * 60,
		Workdays: map[time.Weekday]bool{time.Monday: true, time.Tuesday: true, time.Wednesday: true, time.Thursday: true, time.Friday: true},
		Location: f.loc,
	}
	if len(days) > 0 {
		narrowed := map[time.Weekday]bool{}
		for _, d := range days {
			if prefs.Workdays[d] {
				narrowed[d] = true
			}
		}
		if len(narrowed) == 0 {
			return nil, nil
		}
		prefs.Workdays = narrowed
	}
	raw := calendar.ComputeSlots(f.busy, from, to, dur, prefs, 3)
	var out []schedule.Slot
	for _, s := range raw {
		out = append(out, schedule.Slot{Start: s.Start, End: s.End, Origin: "computed"})
	}
	return out, nil
}

func (f *fakeCalendar) VerifyFree(ctx context.Context, start, end time.Time) (bool, error) {
	return calendar.IsFree(f.busy, start, end.Sub(start)), nil
}

func (f *fakeCalendar) Book(ctx context.Context, summary, description string, start, end time.Time, withMeet bool) (string, string, string, error) {
	fmt.Printf("    ┌─ FAKE CALENDAR: created event %q %s → %s (meet=%v)\n", summary,
		start.In(f.loc).Format("Mon 3:04 PM"), end.In(f.loc).Format("3:04 PM"), withMeet)
	return "fake-event-1", "https://calendar.example/fake-event-1", "https://meet.example/fake", nil
}

func (f *fakeCalendar) CancelEvent(ctx context.Context, eventID string) error {
	fmt.Printf("    ┌─ FAKE CALENDAR: deleted event %s\n", eventID)
	return nil
}

// fakeSender prints instead of sending, and mirrors sends into the DB copy
// the way real self-chat traffic would land there.
type fakeSender struct {
	db   *store.DB
	seq  int
	last string // last self-chat text, for assertions
}

func (f *fakeSender) SendSelf(ctx context.Context, text string) (string, error) {
	f.seq++
	id := fmt.Sprintf("bot-self-%d-%d", runID, f.seq)
	f.last = text
	fmt.Println("  [SELF-CHAT ← commit]")
	for _, line := range strings.Split(text, "\n") {
		fmt.Println("  │ " + line)
	}
	return id, nil
}

func (f *fakeSender) SendTo(ctx context.Context, jid, text string) (string, error) {
	f.seq++
	id := fmt.Sprintf("bot-out-%d-%d", runID, f.seq)
	fmt.Printf("  [→ %s]\n", jid)
	for _, line := range strings.Split(text, "\n") {
		fmt.Println("  │ " + line)
	}
	// Outbound messages land in the store like the real message handler saves them.
	f.db.SaveMessage(&store.Message{
		ID: id, ChatJID: jid, SenderJID: selfJID, SenderName: "Me",
		ChatName: contactName, Content: text, Timestamp: time.Now(), IsFromMe: true,
	})
	return id, nil
}

// debugInterp logs every interpretation so dry-run transcripts show the
// model's reading of the thread.
type debugInterp struct {
	inner schedule.ReplyInterpreter
}

func (d *debugInterp) InterpretReply(ctx context.Context, rc schedule.ReplyContext) (*schedule.Interpretation, error) {
	out, err := d.inner.InterpretReply(ctx, rc)
	if err != nil {
		fmt.Printf("    ~ interp error: %v\n", err)
	} else {
		fmt.Printf("    ~ interp: %+v (thread %d msgs)\n", *out, len(rc.Thread))
	}
	return out, err
}

func (d *debugInterp) InterpretOwnMessage(ctx context.Context, rc schedule.ReplyContext) (bool, error) {
	out, err := d.inner.InterpretOwnMessage(ctx, rc)
	fmt.Printf("    ~ own-message finalized=%v err=%v\n", out, err)
	return out, err
}

var msgSeq int
var runID = time.Now().UnixNano()

func insertMsg(db *store.DB, chatJID, text string, fromMe bool, ts time.Time) string {
	msgSeq++
	id := fmt.Sprintf("dryrun-%d-%d", runID, msgSeq)
	sender := "Me"
	if !fromMe {
		sender = contactName
	}
	// Mirrors handleMessage semantics: chat_name is the CONTACT's name for a
	// 1:1 chat in both directions.
	chatName := ""
	if chatJID == contactJID {
		chatName = contactName
	}
	db.SaveMessage(&store.Message{
		ID: id, ChatJID: chatJID, SenderJID: chatJID, SenderName: sender,
		ChatName: chatName, Content: text, Timestamp: ts, IsFromMe: fromMe,
	})
	return id
}

func userSelf(m *schedule.Manager, db *store.DB, text string, ts time.Time) {
	fmt.Printf("\n  [SELF-CHAT → commit] %q\n", text)
	id := insertMsg(db, selfJID, text, true, ts)
	m.HandleSelfChat(context.Background(), text, id, ts)
}

func contactSays(m *schedule.Manager, db *store.DB, text string, ts time.Time) {
	fmt.Printf("\n  [%s says] %q\n", contactName, text)
	insertMsg(db, contactJID, text, false, ts)
	m.OnContactMessage(context.Background(), contactJID, false, text, ts)
}

func sessionState(db *store.DB) string {
	row, _ := db.GetOpenScheduleSession(contactJID)
	if row == nil {
		return "closed/none"
	}
	return row.State
}

func check(label string, ok bool) {
	mark := "✅"
	if !ok {
		mark = "❌"
	}
	fmt.Printf("  %s %s\n", mark, label)
	if !ok {
		exitCode = 1
	}
}

var exitCode int

func main() {
	dbDir := flag.String("db", "", "directory containing a COPY of commit.db + .crypto_key (never the live ~/.commit)")
	flag.Parse()
	if *dbDir == "" {
		log.Fatal("-db is required (a COPY of ~/.commit — never the live directory)")
	}
	if abs, _ := filepath.Abs(*dbDir); strings.Contains(abs, string(filepath.Separator)+".commit") {
		log.Fatal("refusing to run against what looks like the live ~/.commit directory")
	}
	log.SetFlags(0)

	// Purge artifacts from previous dry runs so threads start clean.
	if raw, err := sql.Open("sqlite", filepath.Join(*dbDir, "commit.db")); err == nil {
		for _, jid := range allContactJIDs {
			raw.Exec("DELETE FROM messages WHERE chat_jid = ? OR id LIKE 'dryrun-%' OR id LIKE 'bot-out-%'", jid)
			raw.Exec("DELETE FROM schedule_sessions WHERE contact_jid = ?", jid)
		}
		raw.Close()
	}

	db, err := store.Open(filepath.Join(*dbDir, "commit.db"))
	if err != nil {
		log.Fatalf("open db copy: %v", err)
	}
	defer db.Close()
	if db.GetAPIKey() == "" {
		log.Fatal("DB copy has no decryptable API key (is .crypto_key present?)")
	}

	loc := time.Local
	now := time.Now()

	// Busy fixtures: pack the next two workdays' mornings.
	var busy []calendar.Interval
	for d := 1; d <= 7; d++ {
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, d)
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		busy = append(busy, calendar.Interval{Start: day.Add(9 * time.Hour), End: day.Add(11 * time.Hour)})
	}

	creds := schedule.Creds{APIKey: db.GetAPIKey, Model: func() string { return "claude-haiku-4-5-20251001" }}
	sender := &fakeSender{db: db}
	m := &schedule.Manager{
		DB:      db,
		Cal:     &fakeCalendar{busy: busy, loc: loc},
		Interp:  &debugInterp{inner: &schedule.LLMInterpreter{Creds: creds}},
		Sender:  sender,
		Creds:   creds,
		SelfJID: func() string { return selfJID },
	}

	fmt.Println("══════ SCENARIO A: happy path — command → propose → accept → yes → book ══════")
	beginScenario(db, 0, "Dryrun Alice")
	userSelf(m, db, "@schedule dryrun alice 30m call this week", now)
	check("session in slots_proposed", sessionState(db) == "slots_proposed")
	check("reply echoes resolved contact name (wrong-contact protection)", strings.Contains(sender.last, contactName))

	userSelf(m, db, "propose", now.Add(1*time.Minute))
	check("session awaiting reply", sessionState(db) == "awaiting_reply")

	userSelf(m, db, "propose", now.Add(2*time.Minute))
	check("double propose within 5 min deduped", strings.Contains(strings.ToLower(sender.last), "already sent"))

	contactSays(m, db, "the first one works for me!", now.Add(10*time.Minute))
	check("reply surfaced with consent prompt", sessionState(db) == "reply_surfaced" && strings.Contains(sender.last, "'yes'"))

	userSelf(m, db, "buy milk, renew passport", now.Add(11*time.Minute)) // personal note
	userSelf(m, db, "yes", now.Add(12*time.Minute))
	check("booked and closed", sessionState(db) == "closed/none")
	check("booking confirmation includes Meet link", strings.Contains(sender.last, "meet.example"))

	fmt.Println("\n══════ SCENARIO B: correction race — counterpart changes answer before 'yes' ══════")
	now = time.Now()
	beginScenario(db, 1, "Dryrun Bob")
	userSelf(m, db, "@schedule dryrun bob 30m call this week", now)
	userSelf(m, db, "propose", now.Add(1*time.Minute))
	contactSays(m, db, "option 1 works", now.Add(5*time.Minute))
	check("acceptance surfaced", sessionState(db) == "reply_surfaced")
	// The change lands but the user doesn't see it and says yes anyway:
	insertMsg(db, contactJID, "wait actually — that slot just got double-booked on my side. the 5:30pm one instead?", false, now.Add(6*time.Minute))
	userSelf(m, db, "yes", now.Add(7*time.Minute))
	check("did NOT book on stale answer (surfaced the change)", sessionState(db) == "reply_surfaced" &&
		strings.Contains(strings.ToLower(sender.last), "moved since"))
	userSelf(m, db, "yes", now.Add(8*time.Minute))
	check("second yes books the corrected slot", sessionState(db) == "closed/none")

	fmt.Println("\n══════ SCENARIO C: consent scoping — a stray 'yes' must do nothing ══════")
	now = time.Now()
	beginScenario(db, 2, "Dryrun Carol")
	userSelf(m, db, "@schedule dryrun carol 30m call this week", now)
	userSelf(m, db, "propose", now.Add(1*time.Minute))
	contactSays(m, db, "the first one works", now.Add(5*time.Minute))
	// Personal note breaks adjacency, then a stray yes 3h later:
	userSelf(m, db, "note to self: ship the deck", now.Add(10*time.Minute))
	userSelf(m, db, "yes", now.Add(3*time.Hour))
	check("stray 'yes' ignored (session still open, nothing booked)", sessionState(db) == "reply_surfaced")
	userSelf(m, db, "@schedule yes", now.Add(3*time.Hour+time.Minute))
	check("prefixed '@schedule yes' books", sessionState(db) == "closed/none")

	fmt.Println("\n══════ SCENARIO D: manual resolution — watcher stands down ══════")
	now = time.Now()
	beginScenario(db, 3, "Dryrun Dan")
	userSelf(m, db, "@schedule dryrun dan 30m call this week", now)
	userSelf(m, db, "propose", now.Add(1*time.Minute))
	// The user settles it themselves in the contact chat:
	txt := "hey, easier to just lock it now — tomorrow 5pm it is, sending you a calendar invite. see you then!"
	fmt.Printf("\n  [ME → %s] %q\n", contactName, txt)
	insertMsg(db, contactJID, txt, true, now.Add(5*time.Minute))
	m.OnContactMessage(context.Background(), contactJID, true, txt, now.Add(5*time.Minute))
	check("watcher stood down silently", sessionState(db) == "closed/none")

	if exitCode == 0 {
		fmt.Println("\nAll dry-run scenarios passed.")
	} else {
		fmt.Println("\nDRY-RUN FAILURES — see ❌ above.")
	}
	os.Exit(exitCode)
}
