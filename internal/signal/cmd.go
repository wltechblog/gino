package signal

import (
	"encoding/json"
)

// FormatSignalJSON returns a pretty-printed JSON representation of a signal.
func FormatSignalJSON(sig Signal) string {
	b, _ := json.MarshalIndent(sig, "", "  ")
	return string(b)
}
