package schedule

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Command is a parsed "@schedule <name> [duration] [format] [window]" line.
// Brackets are optional and order-insensitive after the name.
type Command struct {
	Verb        SessionIntent // schedule / move / cancel
	Name        string
	DurationMin int    // 0 = infer
	Format      string // "", "call", "video", "in-person"
	Window      string // freeform ("this week", "tomorrow", "mon"...)
}

var durationRe = regexp.MustCompile(`^(\d+)\s*(m|min|mins|minutes|h|hr|hrs|hour|hours)$`)

var formatWords = map[string]string{
	"call": "call", "phone": "call",
	"video": "video", "zoom": "video", "meet": "video", "gmeet": "video", "online": "video",
	"in-person": "in-person", "inperson": "in-person", "irl": "in-person",
	"coffee": "in-person", "lunch": "in-person", "dinner": "in-person", "breakfast": "in-person", "walk": "in-person",
}

var windowWords = map[string]bool{
	"today": true, "tomorrow": true, "tmrw": true,
	"mon": true, "monday": true, "tue": true, "tues": true, "tuesday": true,
	"wed": true, "wednesday": true, "thu": true, "thurs": true, "thursday": true,
	"fri": true, "friday": true, "sat": true, "saturday": true, "sun": true, "sunday": true,
	"morning": true, "afternoon": true, "evening": true,
	"week": true, "month": true, "weekend": true,
	"this": true, "next": true, "early": true, "late": true,
}

// ParseCommand parses the text AFTER the "@schedule" prefix.
func ParseCommand(rest string) (*Command, error) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return nil, fmt.Errorf("usage: @schedule <name> [duration] [format] [window]")
	}
	cmd := &Command{Verb: IntentSchedule}
	fields := strings.Fields(rest)
	if len(fields) > 0 {
		switch strings.ToLower(fields[0]) {
		case "move":
			cmd.Verb = IntentMove
			fields = fields[1:]
		case "cancel":
			cmd.Verb = IntentCancel
			fields = fields[1:]
		}
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("who? usage: @schedule %s <name>", cmd.Verb)
	}

	// The name runs until the first token recognizable as duration, format,
	// or window. Everything recognized afterward fills those fields; leftover
	// unrecognized tail tokens extend the window text.
	var nameParts, windowParts []string
	inName := true
	i := 0
	for i < len(fields) {
		tok := strings.ToLower(strings.Trim(fields[i], ",."))

		// duration: "30m", "1h", or "30 min"
		if m := durationRe.FindStringSubmatch(tok); m != nil {
			n, _ := strconv.Atoi(m[1])
			if strings.HasPrefix(m[2], "h") {
				n *= 60
			}
			cmd.DurationMin = n
			inName = false
			i++
			continue
		}
		if n, err := strconv.Atoi(tok); err == nil && i+1 < len(fields) {
			next := strings.ToLower(strings.Trim(fields[i+1], ",."))
			if m := durationRe.FindStringSubmatch("1" + next); m != nil || next == "min" || next == "mins" || next == "minutes" || next == "hours" || next == "hour" {
				if strings.HasPrefix(next, "h") {
					n *= 60
				}
				cmd.DurationMin = n
				inName = false
				i += 2
				continue
			}
		}
		if f, ok := formatWords[tok]; ok {
			cmd.Format = f
			inName = false
			i++
			continue
		}
		if windowWords[tok] {
			windowParts = append(windowParts, tok)
			inName = false
			i++
			continue
		}
		if inName {
			nameParts = append(nameParts, fields[i])
		} else {
			windowParts = append(windowParts, tok)
		}
		i++
	}

	cmd.Name = strings.TrimPrefix(strings.Join(nameParts, " "), "@")
	cmd.Window = strings.Join(windowParts, " ")
	if cmd.Name == "" {
		return nil, fmt.Errorf("who? usage: @schedule <name> [duration] [format] [window]")
	}
	return cmd, nil
}
