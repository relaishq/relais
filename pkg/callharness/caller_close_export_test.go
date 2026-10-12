package callharness

// CloseWithoutDelete closes the caller's PeerConnection (DTLS
// close_notify) without the WHIP DELETE, as a browser closing a tab does.
func (c *Call) CloseWithoutDelete() error { return c.close() }
