package sync

import (
	"bytes"
	"io"
	"strings"
	"time"

	"github.com/launchrctl/launchr"

	"atomicgo.dev/cursor"
	"atomicgo.dev/schedule"
	"github.com/pterm/pterm"
)

func withBoolean(b []bool) bool {
	if len(b) == 0 {
		b = append(b, true)
	}
	return b[0]
}

// Override [pterm.AreaPrinter] to have ability to change area writer.
// @todo remove when it will be fixed in pterm.
type areaPrinter struct {
	RemoveWhenDone bool
	Fullscreen     bool
	Center         bool

	content  string
	isActive bool

	area *cursor.Area
}

// GetContent returns the current area content.
func (p *areaPrinter) GetContent() string {
	return p.content
}

// WithRemoveWhenDone removes the AreaPrinter content after it is stopped.
func (p areaPrinter) WithRemoveWhenDone(b ...bool) *areaPrinter {
	p.RemoveWhenDone = withBoolean(b)
	return &p
}

// WithFullscreen sets the AreaPrinter height the same height as the terminal, making it fullscreen.
func (p areaPrinter) WithFullscreen(b ...bool) *areaPrinter {
	p.Fullscreen = withBoolean(b)
	return &p
}

// WithCenter centers the AreaPrinter content to the terminal.
func (p areaPrinter) WithCenter(b ...bool) *areaPrinter {
	p.Center = withBoolean(b)
	return &p
}

// SetWriter sets the writer for the AreaPrinter.
func (p *areaPrinter) SetWriter(w io.Writer) {
	if writer, ok := w.(cursor.Writer); ok {
		area := p.area.WithWriter(writer)
		p.area = &area
	}
}

// Update overwrites the content of the AreaPrinter.
// Can be used live.
func (p *areaPrinter) Update(text ...any) {
	if p.area == nil {
		newArea := cursor.NewArea()
		p.area = &newArea
	}
	str := pterm.Sprint(text...)
	p.content = str

	if p.Center {
		str = pterm.DefaultCenter.Sprint(str)
	}

	if p.Fullscreen {
		str = strings.TrimRight(str, "\n")
		height := pterm.GetTerminalHeight()
		contentHeight := strings.Count(str, "\n")

		topPadding := 0
		bottomPadding := height - contentHeight - 2

		if p.Center {
			topPadding = (bottomPadding / 2) + 1
			bottomPadding /= 2
		}

		if height > contentHeight {
			str = strings.Repeat("\n", topPadding) + str
			str += strings.Repeat("\n", bottomPadding)
		}
	}
	p.area.Update(str)
}

// Start the AreaPrinter.
func (p *areaPrinter) Start(text ...any) (*areaPrinter, error) {
	p.isActive = true
	str := pterm.Sprint(text...)
	newArea := cursor.NewArea()
	p.area = &newArea

	p.Update(str)

	return p, nil
}

// Stop terminates the AreaPrinter immediately.
// The AreaPrinter will not resolve into anything.
func (p *areaPrinter) Stop() error {
	if !p.isActive {
		return nil
	}
	p.isActive = false
	if p.RemoveWhenDone {
		p.Clear()
	}
	return nil
}

// GenericStart runs Start, but returns a LivePrinter.
// This is used for the interface LivePrinter.
// You most likely want to use Start instead of this in your program.
func (p *areaPrinter) GenericStart() (*pterm.LivePrinter, error) {
	_, _ = p.Start()
	lp := pterm.LivePrinter(p)
	return &lp, nil
}

// GenericStop runs Stop, but returns a LivePrinter.
// This is used for the interface LivePrinter.
// You most likely want to use Stop instead of this in your program.
func (p *areaPrinter) GenericStop() (*pterm.LivePrinter, error) {
	_ = p.Stop()
	lp := pterm.LivePrinter(p)
	return &lp, nil
}

// Clear is a Wrapper function that clears the content of the Area
// moves the cursor to the bottom of the terminal, clears n lines upwards from
// the current position and moves the cursor again.
func (p *areaPrinter) Clear() {
	p.area.Clear()
}

// MultiPrinter overrides [pterm.MultiPrinter] to properly set area writer.
// @todo remove when it will be fixed in pterm.
type MultiPrinter struct {
	IsActive    bool
	Writer      io.Writer
	UpdateDelay time.Duration

	printers []pterm.LivePrinter
	buffers  []*bytes.Buffer
	area     areaPrinter
}

// NewMultiPrinter creates and initializes a new MultiPrinter instance with the provided output writer.
func NewMultiPrinter(areaWriter *launchr.Out) *MultiPrinter {
	newArea := cursor.NewArea()
	area := areaPrinter{
		area: &newArea,
	}
	area.SetWriter(areaWriter)
	multi := &MultiPrinter{
		printers:    []pterm.LivePrinter{},
		Writer:      areaWriter,
		UpdateDelay: time.Millisecond * 200,

		buffers: []*bytes.Buffer{},
		area:    area,
	}

	return multi
}

// SetWriter sets the writer for the AreaPrinter.
func (p *MultiPrinter) SetWriter(writer io.Writer) {
	p.Writer = writer
}

// WithWriter returns a fork of the MultiPrinter with a new writer.
func (p MultiPrinter) WithWriter(writer io.Writer) *MultiPrinter {
	p.Writer = writer
	return &p
}

// WithUpdateDelay returns a fork of the MultiPrinter with a new update delay.
func (p MultiPrinter) WithUpdateDelay(delay time.Duration) *MultiPrinter {
	p.UpdateDelay = delay
	return &p
}

// NewWriter creates new multiWriter buffer.
func (p *MultiPrinter) NewWriter() io.Writer {
	buf := bytes.NewBufferString("")
	p.buffers = append(p.buffers, buf)
	return buf
}

// getString returns all buffers appended and separated by a newline.
func (p *MultiPrinter) getString() string {
	var buffer bytes.Buffer
	for _, b := range p.buffers {
		s := b.String()
		s = strings.Trim(s, "\n")

		parts := strings.Split(s, "\r") // only get the last override
		s = parts[len(parts)-1]

		// check if s is empty, if so get one part before, repeat until not empty
		for s == "" {
			parts = parts[:len(parts)-1]
			s = parts[len(parts)-1]
		}

		s = strings.Trim(s, "\n\r")
		buffer.WriteString(s)
		buffer.WriteString("\n")
	}
	return buffer.String()
}

// Start activates the MultiPrinter, initializes printers, and schedules periodic updates for the area content.
func (p *MultiPrinter) Start() (*MultiPrinter, error) {
	p.IsActive = true
	for _, printer := range p.printers {
		_, _ = printer.GenericStart()
	}

	schedule.Every(p.UpdateDelay, func() bool {
		if !p.IsActive {
			return false
		}

		p.area.Update(p.getString())
		return true
	})

	return p, nil
}

// Stop deactivates the MultiPrinter, stops all associated printers, updates the area content, and terminates the area.
func (p *MultiPrinter) Stop() (*MultiPrinter, error) {
	p.IsActive = false
	for _, printer := range p.printers {
		_, _ = printer.GenericStop()
	}
	time.Sleep(time.Millisecond * 20)
	p.area.Update(p.getString())
	_ = p.area.Stop()

	return p, nil
}

// GenericStart runs Start, but returns a LivePrinter.
// This is used for the interface LivePrinter.
// You most likely want to use Start instead of this in your program.
func (p MultiPrinter) GenericStart() (*pterm.LivePrinter, error) {
	p2, _ := p.Start()
	lp := pterm.LivePrinter(p2)
	return &lp, nil
}

// GenericStop runs Stop, but returns a LivePrinter.
// This is used for the interface LivePrinter.
// You most likely want to use Stop instead of this in your program.
func (p MultiPrinter) GenericStop() (*pterm.LivePrinter, error) {
	p2, _ := p.Stop()
	lp := pterm.LivePrinter(p2)
	return &lp, nil
}
