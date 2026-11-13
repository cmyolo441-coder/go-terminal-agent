package ui

import (
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

// printer delivers lines to the terminal above the live view, strictly in
// order. Commands returned from Update run concurrently in Bubble Tea, so
// printing through them could reorder output; a single goroutine draining a
// queue keeps the transcript in order.
type printer struct {
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []string
	busy    bool
	program *tea.Program
	closed  bool
}

func newPrinter() *printer {
	p := &printer{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *printer) start(prog *tea.Program) {
	p.program = prog
	go p.loop()
}

func (p *printer) print(s string) {
	p.mu.Lock()
	p.queue = append(p.queue, s)
	p.mu.Unlock()
	p.cond.Broadcast()
}

func (p *printer) loop() {
	for {
		p.mu.Lock()
		for len(p.queue) == 0 && !p.closed {
			p.busy = false
			p.cond.Broadcast()
			p.cond.Wait()
		}
		if p.closed && len(p.queue) == 0 {
			p.mu.Unlock()
			return
		}
		s := p.queue[0]
		p.queue = p.queue[1:]
		p.busy = true
		p.mu.Unlock()
		p.program.Println(s)
	}
}

// drain blocks until everything queued has been handed to the program.
func (p *printer) drain() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.queue) > 0 || p.busy {
		p.cond.Wait()
	}
}

func (p *printer) close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.cond.Broadcast()
}
