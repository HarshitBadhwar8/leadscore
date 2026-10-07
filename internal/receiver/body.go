package receiver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/HarshitBadhwar8/leadscore/adapters/apollo"
)

// Body limits (contracts sections 5.4 and 11).
const (
	// maxStringBytes caps every string in a stored body. Long strings are
	// prose (a conversation summary, past transcripts) that nothing automated
	// reads, so the tail of one is cheaper to lose than the event.
	maxStringBytes = 16 << 10
	// maxBodyChars is one Sheets cell, which holds a stored body.
	maxBodyChars = 50_000
	// keptStringRunes caps each kept field of a body already cut down to the
	// parsers' fields, in the rare case that is still too long. 19 fields at
	// this length fit a cell even when every character is escaped.
	keptStringRunes = 256
	// maxRequestBytes is the most the receiver reads of one request. A bigger
	// request gets 413 and is not stored: no Apollo workflow body comes near
	// it, and reading without a bound would let anyone exhaust memory.
	maxRequestBytes = 4 << 20
)

// Marker fields the receiver adds to a body it had to change.
const (
	truncatedFieldsKey = "__truncated_fields" // the paths of strings it shortened
	droppedReasonKey   = "__dropped_reason"   // why only the parsers' fields were kept
	notJSONKey         = "__not_json"
	rawKey             = "raw"
	truncatedMarker    = "… [truncated]"
)

// secretField is the top-level body field that carries the secret when the
// sender cannot set headers (contracts section 5.1).
const secretField = "leadscore_secret"

// incoming is one request body, read as JSON when it is a JSON object.
type incoming struct {
	raw    []byte
	obj    map[string]any // nil when the body is not a JSON object
	secret string         // the body's leadscore_secret, if any
	strip  bool           // the body carried a secret field that must not be stored
}

// readBody decodes a request body and takes the secret field out of it. Every
// top-level key equal to leadscore_secret ignoring case is removed, so no
// spelling of the field reaches the store; only the exact name counts as a
// presented secret.
func readBody(raw []byte) incoming {
	in := incoming{raw: raw}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // numbers stay as sent
	var v any
	if err := dec.Decode(&v); err != nil {
		return in
	}
	if _, err := dec.Token(); err == nil {
		return in // more than one JSON value: not a JSON body
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return in
	}
	in.obj = obj
	for k, val := range obj {
		if !strings.EqualFold(k, secretField) {
			continue
		}
		if k == secretField {
			in.secret, _ = val.(string)
		}
		delete(obj, k)
		in.strip = true
	}
	return in
}

// storedBody is what the receiver stores for one authenticated request
// (contracts section 5.4): the body as sent when it fits and carried no
// secret field; otherwise re-encoded with every string over 16KB shortened;
// and when that is still over a Sheets cell, only the fields the parsers read,
// at their original paths. A body that is not a JSON object is kept as
// {"__not_json": true, "raw": "<first 16KB>"}.
func storedBody(in incoming) []byte {
	if in.obj == nil {
		return notJSONBody(in.raw)
	}
	if !in.strip && utf8.RuneCount(in.raw) <= maxBodyChars && !hasLongString(in.obj) {
		// Retained byte for byte: nothing needed changing.
		return in.raw
	}
	var cut []string
	obj := capStrings(in.obj, "", maxStringBytes, &cut).(map[string]any)
	if len(cut) > 0 {
		sort.Strings(cut)
		obj[truncatedFieldsKey] = cut
	}
	out := encode(obj)
	if utf8.RuneCount(out) <= maxBodyChars {
		return out
	}
	// Still too big for one cell: keep only what the parsers read, so the
	// event and its receiver row survive, and say why the rest went.
	kept := keepPaths(obj, apollo.RequiredPaths())
	kept[droppedReasonKey] = fmt.Sprintf("the body was %d characters after shortening long strings, over the %d a Sheets cell holds; only the fields leadscore reads were kept",
		utf8.RuneCount(out), maxBodyChars)
	out = encode(kept)
	if utf8.RuneCount(out) <= maxBodyChars {
		return out
	}
	// The kept fields alone are too long (each can be 16KB): shorten them
	// further. Keys, emails and ids are far shorter than this.
	var again []string
	kept = capRunes(kept, keptStringRunes, "", &again).(map[string]any)
	return encode(kept)
}

// notJSONBody keeps the first 16KB of a body that is not a JSON object, cut on
// a character boundary and shortened further if escaping makes it too long.
func notJSONBody(raw []byte) []byte {
	n := min(len(raw), maxStringBytes)
	for {
		for n > 0 && n < len(raw) && !utf8.RuneStart(raw[n]) {
			n--
		}
		out := encode(map[string]any{notJSONKey: true, rawKey: string(raw[:n])})
		if utf8.RuneCount(out) <= maxBodyChars || n == 0 {
			return out
		}
		n /= 2
	}
}

// encode writes v as compact JSON without HTML escaping, so text such as
// "<" or "&" keeps its length and spelling.
func encode(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// Values decoded from JSON always encode.
		panic(fmt.Sprintf("receiver: encoding a decoded body: %v", err))
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func hasLongString(v any) bool {
	switch x := v.(type) {
	case string:
		return len(x) > maxStringBytes
	case map[string]any:
		for _, e := range x {
			if hasLongString(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if hasLongString(e) {
				return true
			}
		}
	}
	return false
}

// capStrings shortens every string over limit bytes, recording each one's
// dotted path (array items by index).
func capStrings(v any, path string, limit int, cut *[]string) any {
	switch x := v.(type) {
	case string:
		if s, ok := capText(x, limit); ok {
			*cut = append(*cut, path)
			return s
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = capStrings(e, join(path, k), limit, cut)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = capStrings(e, join(path, strconv.Itoa(i)), limit, cut)
		}
		return x
	}
	return v
}

// capRunes shortens every string over limit characters.
func capRunes(v any, limit int, path string, cut *[]string) any {
	switch x := v.(type) {
	case string:
		if utf8.RuneCountInString(x) <= limit {
			return x
		}
		*cut = append(*cut, path)
		n := 0
		for i := range x {
			if n == limit-utf8.RuneCountInString(truncatedMarker) {
				return x[:i] + truncatedMarker
			}
			n++
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = capRunes(e, limit, join(path, k), cut)
		}
		return x
	}
	return v
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// capText shortens s to limit bytes, marker included, reporting whether it had
// to. The marker is inside the value so a reader of the stored body sees the
// cut without comparing lengths. The cut is on a character boundary: a byte
// cut leaves broken text that reads as ordinary truncation.
func capText(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	cut := limit - len(truncatedMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedMarker, true
}

// keepPaths copies only the given dotted paths of obj, at the same places.
func keepPaths(obj map[string]any, paths []string) map[string]any {
	out := map[string]any{}
	for _, p := range paths {
		parts := strings.Split(p, ".")
		var cur any = obj
		for _, part := range parts {
			m, ok := cur.(map[string]any)
			if !ok {
				cur = nil
				break
			}
			cur, ok = m[part]
			if !ok {
				cur = nil
				break
			}
		}
		if cur == nil {
			continue
		}
		if _, isObj := cur.(map[string]any); isObj {
			continue // the parsers read only leaf values
		}
		dst := out
		for _, part := range parts[:len(parts)-1] {
			next, ok := dst[part].(map[string]any)
			if !ok {
				next = map[string]any{}
				dst[part] = next
			}
			dst = next
		}
		dst[parts[len(parts)-1]] = cur
	}
	return out
}
