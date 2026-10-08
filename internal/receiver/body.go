// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package receiver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/HarshitBadhwar8/leadscore/adapters/apollo"
)

// Body limits (the oversized-body rule and the defaults).
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
	maxRequestBytes = 1 << 20
	// maxListedCuts bounds __truncated_fields, which a body with a long array
	// of long strings could otherwise make as big as the body.
	maxListedCuts = 50
	// maxCutPathRunes caps each listed path: a key can be long too.
	maxCutPathRunes = 128
)

// Marker fields the receiver adds to a body it had to change.
const (
	truncatedFieldsKey = "__truncated_fields" // the paths of strings it shortened
	droppedReasonKey   = "__dropped_reason"   // why only the parsers' fields were kept
	notJSONKey         = "__not_json"
	rawKey             = "raw"
	truncatedMarker    = "… [truncated]"
	redacted           = "[REDACTED]"
)

// secretField is the top-level body field that carries the secret when the
// sender cannot set headers.
const secretField = "leadscore_secret"

// rawSecretField finds a leadscore_secret string field in body text that is
// not valid JSON, any spelling: group 1 is everything up to the value, group
// 2 the value.
var rawSecretField = regexp.MustCompile(`(?i)("leadscore_secret"\s*:\s*")((?:[^"\\]|\\.)*)`)

// maxCandidates bounds how many presented secrets one body may carry, so a
// body cannot make the check hash without limit.
const maxCandidates = 16

// scanSecret returns every top-level leadscore_secret value of a body,
// without decoding anything else: it walks only the first level of the
// object, skipping each other value as raw bytes. This runs before the
// request is authenticated, so it must cost no more than the body itself.
// Every copy is returned, and the request is accepted when any one matches:
// a copy someone smuggled into another field (a prospect's name, say) cannot
// hide the real one.
//
// When the body is not valid JSON, or has anything after the object, the
// field is also looked for in the text, so a body Apollo built with an
// unescaped quote in some value still authenticates (and is stored marked as
// not JSON, its secrets masked).
//
// Unconfirmed: whether Apollo escapes quotes in the variable values it puts
// into a workflow body. If it does not, such bodies are not JSON, and a body
// secret is found by the text match here.
func scanSecret(raw []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return textSecrets(raw)
	}
	var skip json.RawMessage // reused: no value is decoded into Go values
	var found []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return append(found, textSecrets(raw)...)
		}
		key, _ := t.(string)
		if err := dec.Decode(&skip); err != nil {
			return append(found, textSecrets(raw)...)
		}
		var str string
		if key == secretField && json.Unmarshal(skip, &str) == nil && len(found) < maxCandidates {
			found = append(found, str)
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return append(found, textSecrets(raw)...)
	}
	if _, err := dec.Token(); err != io.EOF {
		return append(found, textSecrets(raw)...) // something after the object
	}
	return found
}

// textSecrets are the exact-name leadscore_secret values found in body text.
func textSecrets(raw []byte) []string {
	var out []string
	for _, m := range rawSecretField.FindAllSubmatch(raw, maxCandidates) {
		if bytes.Contains(m[1], []byte(`"`+secretField+`"`)) {
			var s string
			if json.Unmarshal(append(append([]byte{'"'}, m[2]...), '"'), &s) == nil {
				out = append(out, s)
			}
		}
	}
	return out
}

// incoming is one authenticated request body, read as JSON when it is a JSON
// object.
type incoming struct {
	raw     []byte
	obj     map[string]any // nil when the body is not a JSON object
	secrets []string       // the configured secrets, masked wherever they appear
	changed bool           // the body carried a secret that must not be stored
}

// readBody decodes an authenticated request body and takes every secret out
// of it: each key equal to leadscore_secret ignoring case, at any depth, and
// any configured secret's value inside another string.
func readBody(raw []byte, secrets []string) incoming {
	in := incoming{raw: raw}
	for _, s := range secrets {
		if s != "" {
			in.secrets = append(in.secrets, s)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // numbers stay as sent
	var v any
	if err := dec.Decode(&v); err != nil {
		return in
	}
	if _, err := dec.Token(); err != io.EOF {
		return in // anything after the value: not a JSON body
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return in
	}
	in.obj = in.scrub(obj).(map[string]any)
	return in
}

// scrub removes secret fields and masks secret values, recording a change.
func (in *incoming) scrub(v any) any {
	switch x := v.(type) {
	case string:
		for _, s := range in.secrets {
			if strings.Contains(x, s) {
				x = strings.ReplaceAll(x, s, redacted)
				in.changed = true
			}
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if strings.EqualFold(k, secretField) {
				in.changed = true
				continue
			}
			masked := in.scrub(k).(string) // a secret pasted into a key
			out[masked] = in.scrub(e)
		}
		return out
	case []any:
		for i, e := range x {
			x[i] = in.scrub(e)
		}
		return x
	}
	return v
}

// storedBody is what the receiver stores for one authenticated request: the
// body as sent when it fits and carried no
// secret; otherwise re-encoded with every string over 16KB shortened; and
// when that is still over a Sheets cell, only the fields the parsers read, at
// their original paths. A body that is not a JSON object is kept as
// {"__not_json": true, "raw": "<first 16KB>"}, its secrets masked. The result
// always fits one cell.
func storedBody(in incoming) []byte {
	out, _ := storeBody(in)
	return out
}

// Which of storedBody's steps produced the body (for tests).
const (
	stageVerbatim  = "verbatim"
	stageNotJSON   = "not JSON"
	stageCapped    = "strings capped"
	stageKept      = "parser fields kept"
	stageKeptShort = "parser fields shortened"
	stageNonString = "non-strings shortened"
	stageReason    = "reason only"
)

func storeBody(in incoming) ([]byte, string) {
	if in.obj == nil {
		return notJSONBody(in.raw, in.secrets), stageNotJSON
	}
	if !in.changed && utf8.RuneCount(in.raw) <= maxBodyChars && !hasLongString(in.obj) {
		// Retained byte for byte: nothing needed changing.
		return in.raw, stageVerbatim
	}
	var cut []string
	obj := capStrings(in.obj, "", maxStringBytes, &cut).(map[string]any)
	// tryWith encodes m with the list of shortened paths, then without it:
	// the list is dropped before any field is.
	tryWith := func(m map[string]any) ([]byte, bool) {
		if len(cut) > 0 {
			m[truncatedFieldsKey] = listCuts(cut)
			if out := encode(m); fits(out) {
				return out, true
			}
			delete(m, truncatedFieldsKey)
		}
		out := encode(m)
		return out, fits(out)
	}
	out, ok := tryWith(obj)
	if ok {
		return out, stageCapped
	}
	// Still too big for one cell: keep only what the parsers read, so the
	// event and its receiver row survive, and say why the rest went.
	reason := fmt.Sprintf("the body was %d characters after shortening long strings, over the %d a Sheets cell holds; only the fields leadscore reads were kept",
		utf8.RuneCount(out), maxBodyChars)
	kept := keepPaths(obj, apollo.RequiredPaths())
	kept[droppedReasonKey] = reason
	if out, ok = tryWith(kept); ok {
		return out, stageKept
	}
	// The kept fields alone are too long (each can be 16KB, an array can hold
	// many): shorten them further. Keys, emails and ids are far shorter.
	kept = capRunes(keepPaths(obj, apollo.RequiredPaths()), keptStringRunes, "", &cut).(map[string]any)
	kept[droppedReasonKey] = reason
	if out, ok = tryWith(kept); ok {
		return out, stageKeptShort
	}
	// A kept value that is not a string (a huge number, a long array) is
	// still too long: make every such value a short string.
	kept = shortenNonStrings(kept, "", &cut).(map[string]any)
	kept[droppedReasonKey] = reason
	if out, ok = tryWith(kept); ok {
		return out, stageNonString
	}
	return encode(map[string]any{droppedReasonKey: reason + "; even those did not fit, so none were kept"}), stageReason
}

func fits(b []byte) bool { return utf8.RuneCount(b) <= maxBodyChars }

// listCuts is the sorted, de-duplicated list of cut paths, at most
// maxListedCuts of them and then a count of the rest.
func listCuts(cut []string) []string {
	sort.Strings(cut)
	out := make([]string, 0, min(len(cut), maxListedCuts+1))
	for i, c := range cut {
		if i > 0 && c == cut[i-1] {
			continue
		}
		out = append(out, c)
	}
	for i, c := range out {
		if utf8.RuneCountInString(c) > maxCutPathRunes {
			out[i] = runePrefix(c, maxCutPathRunes-utf8.RuneCountInString(truncatedMarker)) + truncatedMarker
		}
	}
	if len(out) > maxListedCuts {
		out = append(out[:maxListedCuts], fmt.Sprintf("and %d more", len(out)-maxListedCuts))
	}
	return out
}

// maskText replaces every configured secret, and the value of any
// leadscore_secret field, in text that is not JSON.
func maskText(raw []byte, secrets []string) []byte {
	for _, s := range secrets {
		for _, form := range secretForms(s) {
			raw = bytes.ReplaceAll(raw, []byte(form), []byte(redacted))
		}
	}
	return rawSecretField.ReplaceAll(raw, []byte("${1}"+redacted))
}

// secretForms are the ways a secret can be spelled in JSON text: as is,
// as JSON escapes it, and with every character as a \uXXXX escape (lower and
// upper case hex), which a decoder reads back as the secret.
func secretForms(s string) []string {
	forms := []string{s}
	if b, err := json.Marshal(s); err == nil {
		forms = append(forms, string(b[1:len(b)-1]))
	}
	var lower, upper strings.Builder
	for _, u := range utf16.Encode([]rune(s)) {
		fmt.Fprintf(&lower, `\u%04x`, u)
		fmt.Fprintf(&upper, `\u%04X`, u)
	}
	return append(forms, lower.String(), upper.String())
}

// notJSONBody keeps the first 16KB of a body that is not a JSON object, its
// secrets masked first (so a cut cannot leave part of one), cut on a
// character boundary and shortened further if escaping makes it too long.
func notJSONBody(raw []byte, secrets []string) []byte {
	raw = maskText(raw, secrets)
	n := min(len(raw), maxStringBytes)
	for {
		for n > 0 && n < len(raw) && !utf8.RuneStart(raw[n]) {
			n--
		}
		out := encode(map[string]any{notJSONKey: true, rawKey: string(raw[:n])})
		if fits(out) || n == 0 {
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

// capRunes shortens every string over limit characters, in objects and
// arrays alike.
func capRunes(v any, limit int, path string, cut *[]string) any {
	switch x := v.(type) {
	case string:
		if utf8.RuneCountInString(x) <= limit {
			return x
		}
		*cut = append(*cut, path)
		return runePrefix(x, limit-utf8.RuneCountInString(truncatedMarker)) + truncatedMarker
	case map[string]any:
		for k, e := range x {
			x[k] = capRunes(e, limit, join(path, k), cut)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = capRunes(e, limit, join(path, strconv.Itoa(i)), cut)
		}
		return x
	}
	return v
}

// shortenNonStrings turns every value that is not a string or an object
// into a short string of its JSON text, when that text is long.
func shortenNonStrings(v any, path string, cut *[]string) any {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = shortenNonStrings(e, join(path, k), cut)
		}
		return x
	}
	text := string(encode(v))
	if utf8.RuneCountInString(text) <= keptStringRunes {
		return v
	}
	*cut = append(*cut, path)
	return runePrefix(text, keptStringRunes-utf8.RuneCountInString(truncatedMarker)) + truncatedMarker
}

// runePrefix is the first n characters of s.
func runePrefix(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
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
