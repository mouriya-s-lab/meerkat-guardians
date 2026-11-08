// Package markers parses the machine markers of contract §2 that the guardians program reads:
// a last line `<!-- pm:<kind> <JSON> -->` whose single-line JSON object carries "v":1.
// The program reads `ask` and the three markers that settle one (`reply`, `replaced`,
// `escalated`); it writes none of them (the plugin does).
package markers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/mouriya-s-lab/meerkat-guardians/internal/config"
)

// Marker is one of Ask, Reply, Replaced, Escalated.
type Marker interface{ marker() }

// Ask is a decision ask written by the delivery coordinator on an implementation issue.
type Ask struct {
	Tier    config.Tier
	Session string
}

// Reply settles ask Ask with a reply; Decision is the council decision ref or nil.
type Reply struct {
	Ask      int64
	Decision *string
}

// Replaced settles ask Ask (nil when no ask was involved) by the new implementation issue By.
type Replaced struct {
	Ask *int64
	By  string
}

// Escalated settles ask Ask for the guardians by raising it as agenda issue Agenda.
type Escalated struct {
	Ask    int64
	Agenda string
}

func (Ask) marker()       {}
func (Reply) marker()     {}
func (Replaced) marker()  {}
func (Escalated) marker() {}

var lineRe = regexp.MustCompile(`^<!-- pm:([a-z]+) (\{.*\}) -->$`)

// split returns the body before the last line and the last line, ignoring trailing whitespace
// and carriage returns.
func split(body string) (rest, last string) {
	body = strings.TrimRight(body, " \t\r\n")
	i := strings.LastIndexByte(body, '\n')
	return body[:i+1], strings.TrimSpace(body[i+1:])
}

// Parse returns the marker on the last line of body. It returns nil, nil when the last line is
// not a pm marker or is a kind the program does not read, and an error when it is a pm marker
// line whose JSON is invalid, lacks "v":1, or has a missing or mistyped field (contract §2:
// such a marker counts as absent).
func Parse(body string) (Marker, error) {
	_, last := split(body)
	m := lineRe.FindStringSubmatch(last)
	if m == nil {
		return nil, nil
	}
	kind := m[1]
	dec := json.NewDecoder(strings.NewReader(m[2]))
	dec.UseNumber()
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("pm:%s: %w", kind, err)
	}
	f := fields{kind: kind, obj: obj}
	if v, err := f.int("v"); err != nil {
		return nil, err
	} else if v != 1 {
		return nil, fmt.Errorf("pm:%s: v is %d, want 1", kind, v)
	}
	switch kind {
	case "ask":
		tier, err := f.str("tier")
		if err != nil {
			return nil, err
		}
		t, ok := config.ParseTier(tier)
		if !ok {
			return nil, fmt.Errorf("pm:ask: tier %q is not simple or complex", tier)
		}
		session, err := f.str("session")
		if err != nil {
			return nil, err
		}
		return Ask{Tier: t, Session: session}, nil
	case "reply":
		ask, err := f.int("ask")
		if err != nil {
			return nil, err
		}
		decision, err := f.nullableStr("decision")
		if err != nil {
			return nil, err
		}
		return Reply{Ask: ask, Decision: decision}, nil
	case "replaced":
		ask, err := f.nullableInt("ask")
		if err != nil {
			return nil, err
		}
		by, err := f.str("by")
		if err != nil {
			return nil, err
		}
		return Replaced{Ask: ask, By: by}, nil
	case "escalated":
		ask, err := f.int("ask")
		if err != nil {
			return nil, err
		}
		agenda, err := f.str("agenda")
		if err != nil {
			return nil, err
		}
		return Escalated{Ask: ask, Agenda: agenda}, nil
	}
	return nil, nil
}

// Strip returns body without its last line when that line is a pm marker, trailing whitespace
// trimmed.
func Strip(body string) string {
	rest, last := split(body)
	if !lineRe.MatchString(last) {
		return strings.TrimRight(body, " \t\r\n")
	}
	return strings.TrimRight(rest, " \t\r\n")
}

type fields struct {
	kind string
	obj  map[string]json.RawMessage
}

func (f fields) raw(key string) (json.RawMessage, error) {
	r, ok := f.obj[key]
	if !ok {
		return nil, fmt.Errorf("pm:%s: %s missing", f.kind, key)
	}
	return r, nil
}

func isNull(r json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(r), []byte("null")) }

func (f fields) int(key string) (int64, error) {
	r, err := f.raw(key)
	if err != nil {
		return 0, err
	}
	dec := json.NewDecoder(bytes.NewReader(r))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return 0, fmt.Errorf("pm:%s: %s: %w", f.kind, key, err)
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, fmt.Errorf("pm:%s: %s is not an integer", f.kind, key)
	}
	i, err := n.Int64()
	if err != nil {
		return 0, fmt.Errorf("pm:%s: %s is not an integer", f.kind, key)
	}
	return i, nil
}

func (f fields) nullableInt(key string) (*int64, error) {
	r, err := f.raw(key)
	if err != nil {
		return nil, err
	}
	if isNull(r) {
		return nil, nil
	}
	i, err := f.int(key)
	if err != nil {
		return nil, err
	}
	return &i, nil
}

func (f fields) str(key string) (string, error) {
	r, err := f.raw(key)
	if err != nil {
		return "", err
	}
	var s string
	if isNull(r) || json.Unmarshal(r, &s) != nil {
		return "", fmt.Errorf("pm:%s: %s is not a string", f.kind, key)
	}
	return s, nil
}

func (f fields) nullableStr(key string) (*string, error) {
	r, err := f.raw(key)
	if err != nil {
		return nil, err
	}
	if isNull(r) {
		return nil, nil
	}
	s, err := f.str(key)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Comment is an issue comment.
type Comment struct {
	ID   int64
	Body string
}

// PendingAsk is an ask comment no marker settles yet; Question is its body without the marker.
type PendingAsk struct {
	Comment  int64
	Tier     config.Tier
	Question string
}

// Invalid is a comment whose last line is a pm marker that does not parse.
type Invalid struct {
	Comment int64
	Err     error
}

// Pending returns, in comment order, the asks among comments that no reply, replaced or
// escalated marker on the same comments settles, and the comments whose marker is invalid.
func Pending(comments []Comment) ([]PendingAsk, []Invalid) {
	var (
		asks    []PendingAsk
		invalid []Invalid
		settled = map[int64]bool{}
	)
	for _, c := range comments {
		m, err := Parse(c.Body)
		if err != nil {
			invalid = append(invalid, Invalid{Comment: c.ID, Err: err})
			continue
		}
		switch m := m.(type) {
		case nil:
		case Ask:
			asks = append(asks, PendingAsk{Comment: c.ID, Tier: m.Tier, Question: Strip(c.Body)})
		case Reply:
			settled[m.Ask] = true
		case Replaced:
			if m.Ask != nil {
				settled[*m.Ask] = true
			}
		case Escalated:
			settled[m.Ask] = true
		default:
			panic(fmt.Sprintf("unhandled marker %T", m))
		}
	}
	out := asks[:0]
	for _, a := range asks {
		if !settled[a.Comment] {
			out = append(out, a)
		}
	}
	return out, invalid
}
