package memhttp

func (t *Transport) Unbind(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.handlers, name)
}
