package ui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/restic/restic/internal/archiver"
	"github.com/restic/restic/internal/restic"
	"github.com/restic/restic/internal/ui/termstatus"
)

type counter struct {
	Files, Dirs uint
	Bytes       uint64
}

type fileWorkerMessage struct {
	filename string
	done     bool
}

// Backup reports progress for the `backup` command.
type Backup struct {
	*Message

	MinUpdatePause time.Duration

	term  *termstatus.Terminal
	v     uint
	start time.Time

	totalBytes uint64

	totalCh     chan counter
	processedCh chan counter
	errCh       chan struct{}
	workerCh    chan fileWorkerMessage
}

// NewBackup returns a new backup progress reporter.
func NewBackup(term *termstatus.Terminal, verbosity uint) *Backup {
	return &Backup{
		Message: NewMessage(term, verbosity),
		term:    term,
		v:       verbosity,
		start:   time.Now(),

		// limit to 60fps by default
		MinUpdatePause: time.Second / 60,

		totalCh:     make(chan counter),
		processedCh: make(chan counter),
		errCh:       make(chan struct{}),
		workerCh:    make(chan fileWorkerMessage),
	}
}

// Run regularly updates the status lines. It should be called in a separate
// goroutine.
func (b *Backup) Run(ctx context.Context) error {
	var (
		lastUpdate       time.Time
		total, processed counter
		errors           uint
		started          bool
		currentFiles     = make(map[string]struct{})
		secondsRemaining uint64
	)

	t := time.NewTicker(time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case t, ok := <-b.totalCh:
			if ok {
				total = t
				started = true
			} else {
				// scan has finished
				b.totalCh = nil
				b.totalBytes = total.Bytes
			}
		case s := <-b.processedCh:
			processed.Files += s.Files
			processed.Dirs += s.Dirs
			processed.Bytes += s.Bytes
			started = true
		case <-b.errCh:
			errors++
			started = true
		case m := <-b.workerCh:
			if m.done {
				delete(currentFiles, m.filename)
			} else {
				currentFiles[m.filename] = struct{}{}
			}
		case <-t.C:
			if !started {
				continue
			}

			if b.totalCh == nil {
				secs := float64(time.Since(b.start) / time.Second)
				todo := float64(total.Bytes - processed.Bytes)
				secondsRemaining = uint64(secs / float64(processed.Bytes) * todo)
			}
		}

		// limit update frequency
		if time.Since(lastUpdate) < b.MinUpdatePause {
			continue
		}
		lastUpdate = time.Now()

		b.update(total, processed, errors, currentFiles, secondsRemaining)
	}
}

// update updates the status lines.
func (b *Backup) update(total, processed counter, errors uint, currentFiles map[string]struct{}, secs uint64) {
	var status string
	if total.Files == 0 && total.Dirs == 0 {
		// no total count available yet
		status = fmt.Sprintf("[%s] %v files, %s, %d errors",
			formatDuration(time.Since(b.start)),
			processed.Files, formatBytes(processed.Bytes), errors,
		)
	} else {
		var eta string

		if secs > 0 {
			eta = fmt.Sprintf(" ETA %s", formatSeconds(secs))
		}

		// include totals
		status = fmt.Sprintf("[%s] %s  %v files %s, total %v files %v, %d errors%s",
			formatDuration(time.Since(b.start)),
			formatPercent(processed.Bytes, total.Bytes),
			processed.Files,
			formatBytes(processed.Bytes),
			total.Files,
			formatBytes(total.Bytes),
			errors,
			eta,
		)
	}

	lines := make([]string, 0, len(currentFiles)+1)
	for filename := range currentFiles {
		lines = append(lines, filename)
	}
	sort.Sort(sort.StringSlice(lines))
	lines = append([]string{status}, lines...)

	b.term.SetStatus(lines)
}

// ErrFn is the error callback function for the archiver, it prints the error and returns nil.
func (b *Backup) ErrFn(item string, fi os.FileInfo, err error) error {
	b.E("error: %v\n", err)
	return nil
}

// StartFile is called when a file is being processed by a worker.
func (b *Backup) StartFile(filename string) {
	b.workerCh <- fileWorkerMessage{
		filename: filename,
	}
}

// CompleteBlob is called for all saved blobs for files.
func (b *Backup) CompleteBlob(filename string, bytes uint64) {
	b.processedCh <- counter{Bytes: bytes}
}

func formatPercent(numerator uint64, denominator uint64) string {
	if denominator == 0 {
		return ""
	}

	percent := 100.0 * float64(numerator) / float64(denominator)

	if percent > 100 {
		percent = 100
	}

	return fmt.Sprintf("%3.2f%%", percent)
}

func formatSeconds(sec uint64) string {
	hours := sec / 3600
	sec -= hours * 3600
	min := sec / 60
	sec -= min * 60
	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, min, sec)
	}

	return fmt.Sprintf("%d:%02d", min, sec)
}

func formatDuration(d time.Duration) string {
	sec := uint64(d / time.Second)
	return formatSeconds(sec)
}

func formatBytes(c uint64) string {
	b := float64(c)
	switch {
	case c > 1<<40:
		return fmt.Sprintf("%.3f TiB", b/(1<<40))
	case c > 1<<30:
		return fmt.Sprintf("%.3f GiB", b/(1<<30))
	case c > 1<<20:
		return fmt.Sprintf("%.3f MiB", b/(1<<20))
	case c > 1<<10:
		return fmt.Sprintf("%.3f KiB", b/(1<<10))
	default:
		return fmt.Sprintf("%d B", c)
	}
}

// CompleteItemFn is the status callback function for the archiver when a
// file/dir has been saved successfully.
func (b *Backup) CompleteItemFn(item string, previous, current *restic.Node, s archiver.ItemStats, d time.Duration) {
	if current == nil {
		return
	}

	switch current.Type {
	case "file":
		b.processedCh <- counter{Files: 1}
		b.workerCh <- fileWorkerMessage{
			filename: item,
			done:     true,
		}
	case "dir":
		b.processedCh <- counter{Dirs: 1}
	}

	if current.Type != "file" {
		return
	}

	b.workerCh <- fileWorkerMessage{
		done:     true,
		filename: item,
	}

	if previous == nil {
		b.D("new       %v, saved in %.3fs (%v added)", item, d.Seconds(), formatBytes(s.DataSize))
		return
	}

	if previous.Equals(*current) {
		b.D("unchanged %v", item)
	} else {
		b.D("modified  %v, saved in %.3fs (%v added)", item, d.Seconds(), formatBytes(s.DataSize))
	}
}

// ReportTotal sets the total stats up to now
func (b *Backup) ReportTotal(item string, s archiver.ScanStats) {
	b.totalCh <- counter{Files: s.Files, Dirs: s.Dirs, Bytes: s.Bytes}

	if item == "" {
		b.V("scan finished in %.3fs", time.Since(b.start).Seconds())
		close(b.totalCh)
		return
	}
}

// Finish prints the finishing messages.
func (b *Backup) Finish() {
	b.V("processed %s in %s", formatBytes(b.totalBytes), formatDuration(time.Since(b.start)))
}
