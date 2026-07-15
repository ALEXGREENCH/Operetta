package proxy

import (
	"net/url"
	"strings"
	"sync"
)

// formStore remembers hidden form fields discovered on origin pages so that
// subsequent POST submissions can be augmented with the expected tokens (e.g. CK/CSRF).
// Entries are stored per logical client and form action and consumed on first use
// to avoid leaking stale tokens across sessions.
type formStore struct {
	mu   sync.Mutex
	data map[formKey]map[string]string
}

type formKey struct {
	client string
	action string
}

const maxFormEntries = 2048

func newFormStore() *formStore {
	return &formStore{data: make(map[formKey]map[string]string)}
}

// Store snapshots the hidden fields for one or more form actions under the given client key.
func (s *formStore) Store(clientKey string, forms map[string]map[string]string) {
	clientKey = strings.TrimSpace(clientKey)
	if clientKey == "" || len(forms) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = make(map[formKey]map[string]string)
	}
	for action, fields := range forms {
		actionKey := normalizeFormActionKey(action)
		if actionKey == "" || len(fields) == 0 {
			continue
		}
		key := formKey{client: clientKey, action: actionKey}
		clone := make(map[string]string, len(fields))
		for name, val := range fields {
			clone[name] = val
		}
		s.data[key] = clone
		for len(s.data) > maxFormEntries {
			for candidate := range s.data {
				delete(s.data, candidate)
				break
			}
		}
	}
}

// Augment merges cached hidden fields into the outgoing form body if none of the
// stored fields are already present. When augmentation succeeds the cached entry
// is consumed to avoid reusing tokens after the POST completes.
func (s *formStore) Augment(clientKey, action, formBody string) (string, bool) {
	clientKey = strings.TrimSpace(clientKey)
	actionKey := normalizeFormActionKey(action)
	if clientKey == "" || actionKey == "" {
		return formBody, false
	}
	vals, err := url.ParseQuery(formBody)
	if err != nil {
		return formBody, false
	}
	key := formKey{client: clientKey, action: actionKey}
	s.mu.Lock()
	defer s.mu.Unlock()
	fields, ok := s.data[key]
	if !ok || len(fields) == 0 {
		return formBody, false
	}
	// Take the entry atomically before returning so two concurrent submissions
	// cannot reuse the same one-time CSRF fields.
	delete(s.data, key)
	applied := false
	for name, val := range fields {
		if _, exists := vals[name]; !exists {
			vals.Set(name, val)
			applied = true
		}
	}
	if applied {
		return vals.Encode(), true
	}
	return formBody, false
}

func normalizeFormActionKey(action string) string {
	action = strings.TrimSpace(action)
	if action == "" {
		return ""
	}
	return normalizeObmlURL(action)
}
