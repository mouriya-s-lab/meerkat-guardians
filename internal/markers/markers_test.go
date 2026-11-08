package markers

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/mouriya-s-lab/meerkat-guardians/internal/config"
)

// format appends a marker line as the plugin writes it: "\n\n<!-- meerkat:<kind> <compact JSON> -->".
func format(t *testing.T, body, kind string, data any) string {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return body + "\n\n<!-- meerkat:" + kind + " " + string(b) + " -->"
}

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		kind string
		data any
		want Marker
	}{
		{"ask", map[string]any{"v": 1, "tier": "simple", "session": "manual"}, Ask{Tier: config.TierSimple, Session: "manual"}},
		{"ask", map[string]any{"v": 1, "tier": "complex", "session": "s1"}, Ask{Tier: config.TierComplex, Session: "s1"}},
		{"reply", map[string]any{"v": 1, "ask": 6696748510, "decision": nil}, Reply{Ask: 6696748510}},
		{"reply", map[string]any{"v": 1, "ask": 7, "decision": "o/a#3"}, Reply{Ask: 7, Decision: new("o/a#3")}},
		{"replaced", map[string]any{"v": 1, "ask": 7, "by": "o/c#9"}, Replaced{Ask: new(int64(7)), By: "o/c#9"}},
		{"replaced", map[string]any{"v": 1, "ask": nil, "by": "o/c#9"}, Replaced{By: "o/c#9"}},
		{"escalated", map[string]any{"v": 1, "ask": 7, "agenda": "o/a#4"}, Escalated{Ask: 7, Agenda: "o/a#4"}},
	}
	for _, c := range cases {
		body := format(t, "要不要把配置改成 TOML？\n第二行", c.kind, c.data)
		got, err := Parse(body)
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: got %#v want %#v", body, got, c.want)
		}
		if s := Strip(body); s != "要不要把配置改成 TOML？\n第二行" {
			t.Fatalf("Strip = %q", s)
		}
		// GitHub web comments use CRLF and may carry trailing whitespace.
		crlf := strings.ReplaceAll(body, "\n", "\r\n") + "\r\n "
		if got, err := Parse(crlf); err != nil || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("CRLF %q: got %#v, %v", crlf, got, err)
		}
		if s := Strip(crlf); s != "要不要把配置改成 TOML？\r\n第二行" {
			t.Fatalf("Strip CRLF = %q", s)
		}
	}
}

func TestParseIgnores(t *testing.T) {
	for _, body := range []string{
		"no marker",
		"",
		"<!-- meerkat:ask {\"v\":1,\"tier\":\"simple\",\"session\":\"s\"} -->\nmarker not on the last line",
		"<!-- meerkat:impl {\"v\":1,\"host\":\"h\",\"pid\":1,\"agenda\":\"a\",\"session\":\"s\",\"file\":\"/f\"} -->",
		"<!-- meerkat:vote {\"v\":1} -->",
		"<!-- a plain html comment -->",
	} {
		m, err := Parse(body)
		if m != nil || err != nil {
			t.Fatalf("%q: got %#v, %v", body, m, err)
		}
		if Strip(body) != strings.TrimRight(body, " \t\r\n") && !strings.HasPrefix(body, "<!-- meerkat:") {
			t.Fatalf("Strip changed %q", body)
		}
	}
}

func TestParseInvalid(t *testing.T) {
	for body, want := range map[string]string{
		`<!-- meerkat:ask {"v":1,"tier":"simple",} -->`:                  "meerkat:ask: invalid character",
		`<!-- meerkat:ask {"tier":"simple","session":"s"} -->`:           "meerkat:ask: v missing",
		`<!-- meerkat:ask {"v":2,"tier":"simple","session":"s"} -->`:     "meerkat:ask: v is 2, want 1",
		`<!-- meerkat:ask {"v":"1","tier":"simple","session":"s"} -->`:   "meerkat:ask: v is not an integer",
		`<!-- meerkat:ask {"v":1,"tier":"urgent","session":"s"} -->`:     `meerkat:ask: tier "urgent" is not simple or complex`,
		`<!-- meerkat:ask {"v":1,"tier":"simple"} -->`:                   "meerkat:ask: session missing",
		`<!-- meerkat:reply {"v":1,"ask":"7","decision":null} -->`:       "meerkat:reply: ask is not an integer",
		`<!-- meerkat:reply {"v":1,"ask":7.5,"decision":null} -->`:       "meerkat:reply: ask is not an integer",
		`<!-- meerkat:reply {"v":1,"ask":7} -->`:                         "meerkat:reply: decision missing",
		`<!-- meerkat:reply {"v":1,"ask":7,"decision":3} -->`:            "meerkat:reply: decision is not a string",
		`<!-- meerkat:replaced {"v":1,"by":"o/c#2"} -->`:                 "meerkat:replaced: ask missing",
		`<!-- meerkat:replaced {"v":1,"ask":7,"by":null} -->`:            "meerkat:replaced: by is not a string",
		`<!-- meerkat:escalated {"v":1,"ask":null,"agenda":"o/a#1"} -->`: "meerkat:escalated: ask is not an integer",
	} {
		m, err := Parse("text\n\n" + body)
		if m != nil || err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %#v, %v; want error containing %q", body, m, err, want)
		}
	}
}

func TestPending(t *testing.T) {
	ask := func(id int64, q, tier string) Comment {
		return Comment{ID: id, Body: format(t, q, "ask", map[string]any{"v": 1, "tier": tier, "session": "w"})}
	}
	comments := []Comment{
		{ID: 1, Body: "plain discussion"},
		ask(10, "Q10 replied", "simple"),
		ask(11, "Q11 replaced", "simple"),
		ask(12, "Q12 escalated", "complex"),
		ask(13, "Q13 open", "complex"),
		{ID: 14, Body: format(t, "答复", "reply", map[string]any{"v": 1, "ask": 10, "decision": nil})},
		{ID: 15, Body: format(t, "理由", "replaced", map[string]any{"v": 1, "ask": 11, "by": "o/c#9"})},
		{ID: 16, Body: format(t, "提为议题", "escalated", map[string]any{"v": 1, "ask": 12, "agenda": "o/a#2"})},
		// A replaced marker without an ask settles nothing.
		{ID: 17, Body: format(t, "council", "replaced", map[string]any{"v": 1, "ask": nil, "by": "o/c#10"})},
		// An invalid reply for 13 does not settle it.
		{ID: 18, Body: "bad\n\n<!-- meerkat:reply {\"ask\":13,\"decision\":null} -->"},
		ask(19, "Q19 open", "simple"),
		// A settlement may precede nothing it names; it must not settle other asks.
		{ID: 20, Body: format(t, "答复", "reply", map[string]any{"v": 1, "ask": 999, "decision": nil})},
	}
	pending, invalid := Pending(comments)
	want := []PendingAsk{
		{Comment: 13, Tier: config.TierComplex, Question: "Q13 open"},
		{Comment: 19, Tier: config.TierSimple, Question: "Q19 open"},
	}
	if !reflect.DeepEqual(pending, want) {
		t.Fatalf("pending = %#v", pending)
	}
	if len(invalid) != 1 || invalid[0].Comment != 18 || !strings.Contains(invalid[0].Err.Error(), "v missing") {
		t.Fatalf("invalid = %#v", invalid)
	}
}
