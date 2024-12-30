package writer

import (
	"RedisShake/internal/entry"
	"RedisShake/internal/log"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type FileFormatWriter string

const (
	AOFWriter  FileFormatWriter = "aof_writer"
	CMDWriter  FileFormatWriter = "cmd_writer"
	JSONWriter FileFormatWriter = "json_writer"
)

var FileFormatWriters = []FileFormatWriter{AOFWriter, CMDWriter, JSONWriter}

type FileWriterOptions struct {
	Filepath string `mapstructure:"filepath" default:""`
}

type fileWriter struct {
	format FileFormatWriter
	path   string
	DbId   int
	ch     chan *entry.Entry
	chWg   sync.WaitGroup
	stat   struct {
		EntryCount int `json:"entry_count"`
	}
}

func (w *fileWriter) Write(e *entry.Entry) {
	w.ch <- e
}

func (w *fileWriter) Close() {
	close(w.ch)
	w.chWg.Wait()
}

func (w *fileWriter) Status() interface{} {
	return w.stat
}

func (w *fileWriter) StatusString() string {
	return fmt.Sprintf("exported entry count=%d", w.stat.EntryCount)
}

func (w *fileWriter) StatusConsistent() bool {
	return true
}

func NewFileWriter(ctx context.Context, opts *FileWriterOptions, format FileFormatWriter) Writer {
	log.Infof("NewFileWriter[%s]: path=[%s]", format, opts.Filepath)
	absolutePath, err := filepath.Abs(opts.Filepath)
	if err != nil {
		log.Panicf("NewFileWriter[%s]: filepath.Abs error: %s", format, err.Error())
	}
	log.Infof("NewFileWriter[%s]: absolute path=[%s]", format, absolutePath)
	w := &fileWriter{
		format: format,
		DbId:   0,
		path:   absolutePath,
		ch:     make(chan *entry.Entry),
	}
	w.stat.EntryCount = 0
	return w
}

func (w *fileWriter) StartWrite(ctx context.Context) (ch chan *entry.Entry) {
	w.chWg = sync.WaitGroup{}
	w.chWg.Add(1)
	go w.processWrite(ctx)
	return w.ch

}

func (w *fileWriter) processWrite(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	file, err := os.Create(w.path)
	if err != nil {
		log.Panicf("create file failed:", err)
		return
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	for {
		select {
		case <-ctx.Done():
			// do nothing until w.ch is closed
		case <-ticker.C:
			writer.Flush()
		case e, ok := <-w.ch:
			if !ok {
				w.chWg.Done()
				writer.Flush()
				return
			}
			w.stat.EntryCount++
			w.writeEntry(writer, e)
		}
	}
}

func (w *fileWriter) writeEntry(writer *bufio.Writer, e *entry.Entry) {
	switch w.format {
	case CMDWriter:
		writer.WriteString(strings.Join(e.Argv, " ") + "\n")
	case AOFWriter:
		writer.Write(e.Serialize())
	case JSONWriter:
		// compute SerializeSize for json result
		e.Serialize()
		json, _ := json.Marshal(e)
		writer.Write(json)
		writer.WriteString("\n")
	}
}
