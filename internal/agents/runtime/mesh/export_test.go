package mesh

import "time"

// WithDiscoveryTTL lets tests shorten how long a detected protocol is trusted.
func WithDiscoveryTTL(d time.Duration) Option {
	return func(c *Client) { c.disc.ttl = d }
}
