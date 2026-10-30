package events

import "sync"

// jobs keeps an on-demand job from running twice at once: two moderators
// pressing the same command, or one pressing it again while it still works.
// The zero value is ready to use.
type jobs struct {
	mu      sync.Mutex
	running map[string]struct{}
}

// begin marks the named job as running. It returns the function that ends it,
// or false when the job is running already.
func (j *jobs) begin(name string) (end func(), ok bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, busy := j.running[name]; busy {
		return nil, false
	}
	if j.running == nil {
		j.running = map[string]struct{}{}
	}
	j.running[name] = struct{}{}
	return func() {
		j.mu.Lock()
		delete(j.running, name)
		j.mu.Unlock()
	}, true
}
