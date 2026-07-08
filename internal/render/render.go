// Package render prints command output.
package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// JSON pretty-prints v. []byte and json.RawMessage are treated as raw JSON
// and re-indented; anything else is marshaled.
func JSON(w io.Writer, v any) error {
	var raw []byte
	switch b := v.(type) {
	case json.RawMessage:
		raw = b
	case []byte:
		raw = b
	default:
		m, err := json.Marshal(v)
		if err != nil {
			return err
		}
		raw = m
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		// Not JSON after all — print verbatim.
		_, werr := w.Write(raw)
		if werr == nil {
			fmt.Fprintln(w)
		}
		return werr
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}
