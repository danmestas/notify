package connectivity

import (
	"encoding/json"
	"sort"
	"strings"
)

// redactedMark replaces a credential value found in text.
const redactedMark = "[redacted]"

// minRedactLen is the shortest credential value redacted. Shorter values
// ("1", "tls") are not secrets and would mangle the text around them.
const minRedactLen = 4

// plainConfigKeys name credential-JSON fields that locate a server rather
// than authenticate to it. Their values stay in probe details because "could
// not reach smtp.example.com:587" is the part an operator acts on.
var plainConfigKeys = map[string]bool{
	"host": true, "addr": true, "base_url": true, "url": true,
	"from": true, "region": true, "port": true, "server": true,
}

// redactCredentials returns text with every credential value in the
// decrypted credential JSON replaced by redactedMark. Values under
// plainConfigKeys are kept. Unparseable credentials redact nothing, since
// there is nothing known to look for.
func redactCredentials(text string, credJSON []byte) string {
	if text == "" {
		return text
	}
	var fields map[string]any
	if json.Unmarshal(credJSON, &fields) != nil {
		return text
	}
	var secrets []string
	collectSecrets(fields, &secrets)
	// Longest first, so a value that contains another is replaced whole.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, s := range secrets {
		text = strings.ReplaceAll(text, s, redactedMark)
	}
	return text
}

func collectSecrets(v any, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if _, isStr := child.(string); isStr && plainConfigKeys[strings.ToLower(k)] {
				continue
			}
			collectSecrets(child, out)
		}
	case []any:
		for _, child := range t {
			collectSecrets(child, out)
		}
	case string:
		if len(t) >= minRedactLen {
			*out = append(*out, t)
		}
	}
}
