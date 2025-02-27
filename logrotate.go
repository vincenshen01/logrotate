package logrotate

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vincenshen01/crontab"
)

// ensure Logger always implement io.WriteCloser
var _ io.WriteCloser = (*Logger)(nil)

// Logger is a logger with rotation function
type Logger struct {
	opts *Options
	file *os.File
	cron *crontab.Crontab
	size int64
	mu   sync.Mutex
	once sync.Once
	ch   chan bool
}

// NewLogger creates a new logger
func NewLogger(opts ...Option) (*Logger, error) {
	var options Options
	for _, o := range opts {
		o(&options)
	}
	if err := options.Apply(); err != nil {
		return nil, err
	}
	println("logs to " + options.File)
	logger := &Logger{opts: &options}
	if options.cron != "" {
		logger.cron = crontab.New()
		logger.cron.Add("rotate log", options.cron, logger.Rotate)
	}
	return logger, nil
}

// Write writes content into file.
// If the length of the content is greater than RotateSize, an error is returned.
func (l *Logger) Write(bs []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	wl := int64(len(bs))
	if l.shouldRotate(wl) {
		err = fmt.Errorf("write length(%d) exceeds maximum file size(%d)", wl, l.opts.rotateSize)
		return
	}
	if l.file == nil {
		if err = l.openFile(wl); err != nil {
			return
		}
	}
	if l.shouldRotate(wl + l.size) {
		if err = l.rotate(); err != nil {
			return
		}
	}
	n, err = l.file.Write(bs)
	l.size += int64(n)
	return
}

// Close closes file resource
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	return l.close()
}

func (l *Logger) close() error {
	if l.file == nil {
		return nil
	}
	return l.file.Close()
}

func (l *Logger) shouldRotate(size int64) bool {
	return l.opts.rotateSize > 0 && size > l.opts.rotateSize
}

func (l *Logger) openFile(length int64) (err error) {
	info, err := os.Stat(l.opts.File)
	if os.IsNotExist(err) {
		return l.openNewFile()
	}
	if err != nil {
		return
	}
	if l.shouldRotate(info.Size() + length) {
		return l.rotate()
	}
	file, err := os.OpenFile(l.opts.File, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	l.file = file
	l.size = info.Size()
	return
}

func (l *Logger) openNewFile() error {
	dir := filepath.Dir(l.opts.File)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("can't make directories for new logfile, error(%v)", err)
	}
	mode := os.FileMode(0644)
	info, err := os.Stat(l.opts.File)
	if err == nil {
		mode = info.Mode()
		an := archiveName(l.opts.File, l.opts.ArchiveTimeFormat)
		if err = os.Rename(l.opts.File, an); err != nil {
			return fmt.Errorf("can't archive(%s) error(%v)", an, err)
		}
	}
	f, err := os.OpenFile(l.opts.File, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("can't open new log file(%s) error(%v)", l.opts.File, err)
	}
	l.file = f
	l.size = 0
	return nil
}

// Rotate cut logs by rules
func (l *Logger) Rotate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rotate()
}

func (l *Logger) rotate() error {
	if err := l.close(); err != nil {
		return err
	}
	if err := l.openNewFile(); err != nil {
		return err
	}
	l.once.Do(func() {
		l.ch = make(chan bool, 1)
		go func() {
			for range l.ch {
				if e := l.handleArchives(); e != nil {
					println(e.Error())
				}
			}
		}()
	})
	select {
	case l.ch <- true:
	default:
	}
	return nil
}

func (l *Logger) handleArchives() error {
	var lastErr error
	if l.opts.rotateSize == 0 && l.opts.cron == "" {
		return nil
	}
	files, err := l.archives()
	if err != nil {
		return err
	}
	var remove, remain []logFile
	if l.opts.MaxArchiveDays > 0 {
		cutoff := time.Now().Add(-1 * l.opts.maxArchiveDuration)
		for _, f := range files {
			if f.timestamp.Before(cutoff) {
				remove = append(remove, f)
			} else {
				remain = append(remain, f)
			}
		}
	}
	if l.opts.MaxArchives > 0 && len(remain) > l.opts.MaxArchives {
		remove = append(remove, remain[l.opts.MaxArchives:]...)
		remain = remain[:l.opts.MaxArchives]
	}
	for _, f := range remove {
		if err := os.Remove(filepath.Join(f.dir, f.Name())); err != nil {
			lastErr = err
		}
	}
	if l.opts.Compress {
		for _, f := range remain {
			if strings.HasSuffix(f.Name(), compressSuffix) {
				continue
			}
			path := filepath.Join(f.dir, f.Name())
			if err := compressFile(path); err != nil {
				lastErr = err
			}
		}
	}
	return lastErr
}

func (l *Logger) archives() ([]logFile, error) {
	dir := filepath.Dir(l.opts.File)
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var (
		t           time.Time
		logs        []logFile
		tf          = l.opts.ArchiveTimeFormat
		prefix, ext = splitFilename(l.opts.File)
	)
	prefix += "-"
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if t, err = timeFromName(tf, f.Name(), prefix, ext); err == nil {
			logs = append(logs, logFile{f, t, dir})
			continue
		}
		if t, err = timeFromName(tf, f.Name(), prefix, ext+compressSuffix); err == nil {
			logs = append(logs, logFile{f, t, dir})
			continue
		}
	}
	sort.Sort(byTime(logs))
	return logs, nil
}

type logFile struct {
	os.FileInfo
	timestamp time.Time
	dir       string
}

type byTime []logFile

func (bt byTime) Less(i, j int) bool {
	return bt[i].timestamp.After(bt[j].timestamp)
}

func (bt byTime) Swap(i, j int) {
	bt[i], bt[j] = bt[j], bt[i]
}

func (bt byTime) Len() int {
	return len(bt)
}
