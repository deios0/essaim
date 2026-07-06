package bus

import "context"

// Zone returns the SERVER-enforced zone for this key, read off an event the key
// is permitted to see (the bus filters events to the caller's zone). This is the
// authoritative zone — NOT any client-supplied --zone label, which the server
// ignores. Returns "" (no error) when the zone has no events yet to confirm it.
func (c *Client) Zone(ctx context.Context) (string, error) {
	evs, _, err := c.pollLimited(ctx, 0, 1) // earliest event; its zone == our zone
	if err != nil {
		return "", err
	}
	for _, e := range evs {
		if e.Zone != "" {
			return e.Zone, nil
		}
	}
	return "", nil
}
