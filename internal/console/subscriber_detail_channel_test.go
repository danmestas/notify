package console

import "testing"

// TestChannelTypeForProvider_Mastodon proves mastodon is registered as a chat
// provider so the console can store a per-subscriber @handle address for it
// (channelCredentials/channelTypeForProvider drive delivery/chat.go's
// credentials.address lookup). Bluesky is intentionally absent: it has no
// per-subscriber address, only public broadcasts.
func TestChannelTypeForProvider_Mastodon(t *testing.T) {
	if got := channelTypeForProvider("mastodon"); got != "chat" {
		t.Errorf("channelTypeForProvider(mastodon) = %q, want %q", got, "chat")
	}
}

func TestChannelTypeForProvider_BlueskyHasNoAddress(t *testing.T) {
	if got := channelTypeForProvider("bluesky"); got != "" {
		t.Errorf("channelTypeForProvider(bluesky) = %q, want empty (no per-subscriber address)", got)
	}
}
