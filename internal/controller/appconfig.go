package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// decodeObject parses JSON text into a map, keeping numbers as written. Empty
// text is an empty object.
func decodeObject(text string) (map[string]any, error) {
	if strings.TrimSpace(text) == "" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if v == nil {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("not a JSON object")
	}
	return m, nil
}

// mergeAppConfig sets the top-level keys of desired over the live config text.
// It returns the merged config and whether it differs from live.
func mergeAppConfig(live string, desired []byte) (merged string, changed bool, err error) {
	liveMap, err := decodeObject(live)
	if err != nil {
		return "", false, fmt.Errorf("live config: %w", err)
	}
	want, err := decodeObject(string(desired))
	if err != nil {
		return "", false, fmt.Errorf("spec config: %w", err)
	}
	for k, v := range want {
		if cur, ok := liveMap[k]; !ok || !jsonEqual(cur, v) {
			changed = true
		}
		liveMap[k] = v
	}
	if !changed {
		return live, false, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(liveMap); err != nil {
		return "", false, err
	}
	return strings.TrimSpace(buf.String()), true, nil
}

// jsonEqual compares decoded JSON values; numbers by value (30 equals 30.0).
func jsonEqual(a, b any) bool {
	an, aok := a.(json.Number)
	bn, bok := b.(json.Number)
	if aok || bok {
		if !aok || !bok {
			return false
		}
		af, aerr := strconv.ParseFloat(an.String(), 64)
		bf, berr := strconv.ParseFloat(bn.String(), 64)
		if aerr != nil || berr != nil {
			return an == bn
		}
		return af == bf
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, x := range av {
			y, ok := bv[k]
			if !ok || !jsonEqual(x, y) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}
