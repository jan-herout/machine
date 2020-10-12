//go:generate godocdown -template docs.template -o README.md

package machine

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// Machine is a zero dependency runtime for managed goroutines. It is inspired by errgroup.Group with extra bells & whistles:
type Machine struct {
	parent    *Machine
	children  []*Machine
	childMu   sync.RWMutex
	cache     Cache
	done      chan struct{}
	cancel    func()
	ctx       context.Context
	workQueue chan *work
	mu        sync.RWMutex
	routines  map[int]Routine
	max       int
	closeOnce sync.Once
	doneOnce  sync.Once
	pubsub    PubSub
	total     int64
}

// New Creates a new machine instance with the given root context & options
func New(ctx context.Context, options ...Opt) *Machine {
	opts := &option{}
	for _, o := range options {
		o(opts)
	}
	if opts.maxRoutines <= 0 {
		opts.maxRoutines = 10000
	}
	if opts.cache == nil {
		opts.cache = &cache{data: &sync.Map{}}
	}
	if opts.pubsub == nil {
		opts.pubsub = &pubSub{
			subscriptions: map[string]map[int]chan interface{}{},
			subMu:         sync.RWMutex{},
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &Machine{
		parent:    opts.parent,
		children:  opts.children,
		cache:     opts.cache,
		done:      make(chan struct{}, 1),
		cancel:    cancel,
		ctx:       ctx,
		workQueue: make(chan *work),
		mu:        sync.RWMutex{},
		routines:  map[int]Routine{},
		max:       opts.maxRoutines,
		closeOnce: sync.Once{},
		doneOnce:  sync.Once{},
		pubsub:    opts.pubsub,
		total:     0,
	}
	go m.serve()
	return m
}

// Cache returns the machines Cache implementation
func (m *Machine) Cache() Cache {
	return m.cache
}

// Current returns current managed goroutine count
func (p *Machine) Current() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	current := len(p.routines)
	for _, child := range p.children {
		current += child.Current()
	}
	return current
}

// Total returns total goroutines that have been executed by the machine
func (p *Machine) Total() int {
	return int(atomic.LoadInt64(&p.total))
}

// Go calls the given function in a new goroutine.
//
// The first call to return a non-nil error who's cause is machine.Cancel cancels the context of every job.
// All errors that are not of type machine.Cancel will be returned by Wait.
func (m *Machine) Go(fn Func, opts ...GoOpt) {
	o := &goOpts{}
	for _, opt := range opts {
		opt(o)
	}
	if m.ctx.Err() == nil {
		m.workQueue <- &work{
			opts: o,
			fn:   fn,
		}
	}
}

func (m *Machine) serve() {
	for {
		select {
		case <-m.done:
			return
		case w := <-m.workQueue:
			if len(w.opts.middlewares) > 0 {
				for _, ware := range w.opts.middlewares {
					w.fn = ware(w.fn)
				}
			}
			for x := m.Current(); x >= m.max; x = m.Current() {

			}
			if w.opts.id == 0 {
				w.opts.id = rand.Int()
			}
			var (
				child  context.Context
				cancel func()
			)
			if w.opts.timeout != nil {
				child, cancel = context.WithTimeout(m.ctx, *w.opts.timeout)
			} else {
				child, cancel = context.WithCancel(m.ctx)
			}
			routine := &goRoutine{
				machine:  m,
				ctx:      child,
				id:       w.opts.id,
				tags:     w.opts.tags,
				start:    time.Now(),
				doneOnce: sync.Once{},
				cancel:   cancel,
			}
			m.mu.Lock()
			m.routines[w.opts.id] = routine
			m.mu.Unlock()
			atomic.AddInt64(&m.total, 1)
			go func() {
				defer func() {
					r := recover()
					if _, ok := r.(error); ok {
						fmt.Println("machine: panic recovered")
					}
				}()
				defer routine.done()
				w.fn(routine)
			}()
		}
	}
}

// Wait blocks until total active goroutine count reaches zero for the instance and all of it's children.
func (m *Machine) Wait() {
	for m.Current() > 0 {
		for len(m.workQueue) > 0 {
		}
	}
}

// Cancel cancels every goroutines context
func (p *Machine) Cancel() {
	p.closeOnce.Do(func() {
		if p.cancel != nil {
			p.cancel()
			for _, child := range p.children {
				child.Close()
			}
		}
	})
}

// Stats returns Goroutine information from the machine
func (m *Machine) Stats() *Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	copied := []RoutineStats{}
	for _, v := range m.routines {
		if v != nil {
			copied = append(copied, RoutineStats{
				PID:      v.PID(),
				Start:    v.Start(),
				Duration: v.Duration(),
				Tags:     v.Tags(),
			})
		}
	}
	return &Stats{
		TotalRoutines: len(copied),
		Routines:      copied,
		TotalChildren: len(m.children),
		HasParent:     m.parent != nil,
	}
}

// Close cleans up the machine instance and all of it's children.
func (m *Machine) Close() {
	m.doneOnce.Do(func() {
		m.Cancel()
		m.done <- struct{}{}
		m.cache.Close()
		m.pubsub.Close()
		for _, child := range m.children {
			child.Close()
		}
	})
}

// Sub returns a nested Machine instance.
func (m *Machine) Sub(opts ...Opt) *Machine {
	opts = append(opts, WithParent(m))
	sub := New(m.ctx, opts...)
	m.childMu.Lock()
	m.children = append(m.children, sub)
	m.childMu.Unlock()
	return sub
}

// Parent returns the parent Machine instance if it exists.
func (m *Machine) Parent() *Machine {
	return m.parent
}
