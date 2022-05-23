package sacura

import (
	"fmt"
	"sort"
	"sync"

	ce "github.com/cloudevents/sdk-go/v2"
	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	unknownPartitionKey = "unknown"
)

type StateManager struct {
	lock     sync.RWMutex
	received map[string][]string
	sent     map[string][]string
	config   StateManagerConfig

	terminated bool
	metrics    Metrics
}

type StateManagerConfig struct {
	Ordered bool
	OrderedConfig
}

func StateManagerConfigFromConfig(config Config) StateManagerConfig {
	if config.Ordered != nil {
		return StateManagerConfig{
			OrderedConfig: *config.Ordered,
			Ordered:       true,
		}
	}
	return StateManagerConfig{Ordered: false}
}

func NewStateManager(config StateManagerConfig) *StateManager {
	return &StateManager{
		received: make(map[string][]string),
		sent:     make(map[string][]string),
		config:   config,
	}
}

func (s *StateManager) ReadSent(sent <-chan ce.Event) <-chan struct{} {
	sg := make(chan struct{})
	go func(set *StateManager) {
		for e := range sent {
			func() {
				s.lock.RLock()
				defer s.lock.RUnlock()
				insert(&e, s.sent, &s.config)
			}()
		}
		sg <- struct{}{}
	}(s)
	return sg
}

func (s *StateManager) ReadReceived(received <-chan ce.Event) <-chan struct{} {
	sg := make(chan struct{})
	go func(set *StateManager) {
		for e := range received {
			func() {
				s.lock.RLock()
				defer s.lock.RUnlock()
				insert(&e, s.received, &s.config)
			}()
		}
		sg <- struct{}{}
	}(s)
	return sg
}

func insert(e *ce.Event, store map[string][]string, config *StateManagerConfig) {
	pk := unknownPartitionKey
	if config.Ordered {
		extenstions := e.Extensions()
		if v, ok := extenstions["partitionkey"]; ok {
			pk = v.(string)
		}
	}
	if _, ok := store[pk]; !ok {
		store[pk] = make([]string, 0, 100)
	}
	store[pk] = append(store[pk], e.ID())
}

func (s *StateManager) ReceivedCount() int {
	s.lock.RLock()
	defer s.lock.RUnlock()

	count := 0
	for _, v := range s.received {
		count += len(v)
	}
	return count
}

func (s *StateManager) Diff() string {
	s.lock.RLock()
	defer s.lock.RUnlock()

	hasDiff := false
	fullDiff := "Diff by partition key\n"

	for k, v := range s.sent {
		sent := v
		var received []string
		if v, ok := s.received[k]; ok {
			received, _ = removeDuplicates(v) // at least once TODO configurable delivery guarantee
		}

		if !s.config.Ordered {
			sort.Strings(sent)
			sort.Strings(received)
		}

		diff := cmp.Diff(received, sent)
		if diff != "" {
			hasDiff = true
		}
		fullDiff += fmt.Sprintf("partitionkey: '%s' (-want, +got)\n%s", k, diff)
	}

	if !hasDiff {
		return ""
	}
	return fullDiff
}

func (s *StateManager) GenerateReport() Report {
	s.lock.RLock()
	defer s.lock.RUnlock()

	r := Report{
		LostCount:                     0,
		Metrics:                       s.metrics,
		LostEventsByPartitionKey:      make(map[string][]string, 8),
		DuplicateEventsByPartitionKey: make(map[string][]string, 8),
		ReceivedEventsByPartitionKey:  make(map[string][]string, 8),
		Terminated:                    s.terminated,
	}

	for k, v := range s.sent {
		sent := v
		var received []string
		var duplicates []string
		if v, ok := s.received[k]; ok {
			received, duplicates = removeDuplicates(v) // at least once TODO configurable delivery guarantee
		}

		if !s.config.Ordered {
			sort.Strings(sent)
			sort.Strings(received)
			sort.Strings(duplicates)
		}

		diff := sets.NewString(sent...).Difference(sets.NewString(received...))
		r.LostEventsByPartitionKey[k] = diff.List()
		r.LostCount += len(r.LostEventsByPartitionKey[k])
		r.DuplicateEventsByPartitionKey[k] = duplicates
		r.DuplicateCount += len(duplicates)
		r.ReceivedEventsByPartitionKey[k] = v
		r.ReceivedCount += len(v)
	}

	return r
}

func (s *StateManager) Terminated(metrics Metrics) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.terminated = true
	s.metrics = metrics
}

func removeDuplicates(a []string) ([]string, []string) {
	t := make(map[string]struct{})
	result := make([]string, 0, len(a))
	duplicates := make([]string, 0, len(a))
	for _, v := range a {
		if _, ok := t[v]; !ok {
			result = append(result, v)
		} else {
			duplicates = append(duplicates, v)
		}
		t[v] = struct{}{}
	}
	return result, duplicates
}
