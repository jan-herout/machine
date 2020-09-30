//go:generate godocdown -o README.md

package machine

import (
	"context"
	"crypto/rand"
	"fmt"
	"github.com/pkg/errors"
	"log"
	"sync"
	"time"
)

var Cancel = errors.New("[machine] cancel")

// Machine is just like sync.WaitGroup, except it lets you throttle max goroutines.
type Machine struct {
	cancel        func()
	ctx           context.Context
	errs          []error
	routineMu     sync.RWMutex
	routines      map[string]Routine
	max           int
	closeOnce     sync.Once
	debug         bool
	workerChan    chan *worker
	publishMu     sync.RWMutex
	publishChan   map[string]chan interface{}
	subMu         sync.RWMutex
	subscriptions map[string]map[string]chan interface{}
}

type worker struct {
	fn   func(routine Routine) error
	tags []string
}

type Routine interface {
	Context() context.Context
	ID() string
	Tags() []string
	Start() time.Time
	Duration() time.Duration
	Publish(channel string, obj interface{})
	Subscribe(channel string) chan interface{}
}

type goRoutine struct {
	machine *Machine
	ctx     context.Context
	id      string
	tags    []string
	start   time.Time
}

func (r *goRoutine) Context() context.Context {
	return r.ctx
}

func (r *goRoutine) ID() string {
	return r.id
}

func (r *goRoutine) Tags() []string {
	return r.Tags()
}

func (r *goRoutine) Start() time.Time {
	return r.start
}

func (r *goRoutine) Duration() time.Duration {
	return time.Since(r.start)
}

func (g *goRoutine) Publish(channel string, obj interface{}) {
	g.machine.publishMu.Lock()
	defer g.machine.publishMu.Unlock()
	if g.machine.publishChan == nil {
		g.machine.publishChan = map[string]chan interface{}{}
	}
	if g.machine.publishChan[channel] == nil {
		g.machine.publishChan[channel] = make(chan interface{})
	}
	g.machine.publishChan[channel] <- obj
}

func (g *goRoutine) Subscribe(channel string) chan interface{} {
	g.machine.subMu.Lock()
	defer g.machine.subMu.Unlock()
	ch := make(chan interface{})
	if g.machine.subscriptions == nil {
		g.machine.subscriptions = map[string]map[string]chan interface{}{}
	}
	if g.machine.subscriptions[channel] == nil {
		g.machine.subscriptions[channel] = map[string]chan interface{}{}
	}
	g.machine.subscriptions[channel][g.id] = ch
	return ch
}

type Opts struct {
	MaxRoutines int
	Debug       bool
}

func New(ctx context.Context, opts *Opts) (*Machine, error) {
	if opts == nil {
		opts = &Opts{}
	}
	if opts.MaxRoutines <= 0 {
		opts.MaxRoutines = 10000
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &Machine{
		cancel:        cancel,
		ctx:           ctx,
		errs:          nil,
		routineMu:     sync.RWMutex{},
		routines:      map[string]Routine{},
		max:           opts.MaxRoutines,
		closeOnce:     sync.Once{},
		debug:         false,
		workerChan:    make(chan *worker, opts.MaxRoutines),
		publishMu:     sync.RWMutex{},
		publishChan:   map[string]chan interface{}{},
		subMu:         sync.RWMutex{},
		subscriptions: map[string]map[string]chan interface{}{},
	}
	child1, cancel1 := context.WithCancel(m.ctx)
	workerLisRoutine := m.addRoutine(child1, "machine.worker.queue")
	m.routineMu.Lock()
	m.routines[workerLisRoutine.ID()] = workerLisRoutine
	m.routineMu.Unlock()
	go func() {
		defer m.closeRoutine(workerLisRoutine.ID())
		defer cancel1()
		for {
			select {
			case <-child1.Done():
				return
			case work := <-m.workerChan:
				child2, cancel2 := context.WithCancel(workerLisRoutine.Context())
				workerRoutine := m.addRoutine(child2, work.tags...)
				m.routineMu.Lock()
				m.routines[workerRoutine.ID()] = workerRoutine
				m.routineMu.Unlock()
				go func() {
					defer m.closeRoutine(workerRoutine.ID())
					defer cancel2()
					if err := work.fn(workerRoutine); err != nil {
						if errors.Cause(err) == Cancel {
							m.Cancel()
						} else {
							m.addErr(err)
						}
					}
				}()
			}
		}
	}()
	return m, nil
}

// Current returns current managed goroutine count
func (p *Machine) Current() int {
	p.routineMu.Lock()
	defer p.routineMu.Unlock()
	return len(p.routines)
}

func (p *Machine) addRoutine(ctx context.Context, tags ...string) Routine {
	var x int
	for x = len(p.routines); x >= p.max && p.ctx.Err() == nil; x = p.Current() {
	}
	id := uuid()
	return &goRoutine{
		machine: p,
		ctx:     ctx,
		id:      id,
		tags:    tags,
		start:   time.Now(),
	}
}

// Go calls the given function in a new goroutine.
//
// The first call to return a non-nil error who's cause is CancelGroup cancels the context of every job.
// All errors that are not CancelGroup will be returned by Wait.
func (p *Machine) Go(f func(routine Routine) error, tags ...string) {
	p.workerChan <- &worker{
		fn:   f,
		tags: tags,
	}
}

func (p *Machine) addErr(err error) {
	p.errs = append(p.errs, err)
}

func (p *Machine) closeRoutine(id string) {
	p.routineMu.Lock()
	delete(p.routines, id)
	p.routineMu.Unlock()
}

func (p *Machine) Wait() []error {
	for !(p.Current() > 0) {
	}
	p.Cancel()
	return p.errs
}

// Cancel cancels every functions context
func (p *Machine) Cancel() {
	p.closeOnce.Do(func() {
		if p.cancel != nil {
			p.cancel()
		}
	})
}

type Stats struct {
	Count    int
	Routines map[string]Routine
}

func (m *Machine) Stats() Stats {
	copied := map[string]Routine{}
	m.routineMu.RLock()
	defer m.routineMu.RUnlock()
	for k, v := range m.routines {
		if v != nil {
			copied[k] = v
		}
	}
	return Stats{
		Routines: copied,
		Count:    len(copied),
	}
}

func (p *Machine) debugf(format string, a ...interface{}) {
	if p.debug {
		fmt.Printf(format, a...)
	}
}

func uuid() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		log.Fatal(err)
	}
	uuid := fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
	return uuid
}
