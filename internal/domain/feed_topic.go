package domain

import (
	"fmt"
	"strings"
)

// maxFeedTopicLen bounds the topic string ValidateFeedTopic accepts.
const maxFeedTopicLen = 128

// ValidateFeedTopic is the subject-injection gate for stream-channel topics.
// It lives in domain (a dependency-free leaf) rather than eventbus so both
// the "stream" step-type's static authoring validation and the delivery
// package's HandleStream (which must not import eventbus) can call it
// directly; eventbus.PublishFeed/ConsumeFeed also call it, as defense in
// depth against a crafted or mis-rendered topic reaching a NATS subject.
//
// A topic must be non-empty, at most 128 bytes, dot-separated into
// non-empty tokens (no leading/trailing/double dots), each token matching
// ^[A-Za-z0-9_-]+$ — which rejects NATS wildcards ("*", ">"), whitespace,
// "$" (JetStream/system subject prefixes), unicode, and NUL bytes.
func ValidateFeedTopic(topic string) error {
	if topic == "" {
		return fmt.Errorf("domain: feed topic must be non-empty")
	}
	if len(topic) > maxFeedTopicLen {
		return fmt.Errorf("domain: feed topic exceeds %d bytes", maxFeedTopicLen)
	}
	tokens := strings.Split(topic, ".")
	for _, tok := range tokens {
		if tok == "" {
			return fmt.Errorf("domain: feed topic %q has an empty segment", topic)
		}
		for _, r := range tok {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
				// allowed
			default:
				return fmt.Errorf("domain: feed topic %q contains disallowed character %q", topic, r)
			}
		}
	}
	return nil
}
