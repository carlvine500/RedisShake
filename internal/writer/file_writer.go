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
	"strconv"
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
	Filepath        string `mapstructure:"filepath" default:""`
	GroupByPrefix   bool   `mapstructure:"group_by_prefix" default:"false"`
	PrefixDelimiter string `mapstructure:"prefix_delimiter" default:":"`
}

type fileWriter struct {
	format          FileFormatWriter
	path            string
	groupByPrefix   bool
	prefixDelemiter string
	ch              chan *entry.Entry
	chWg            sync.WaitGroup
	stat            struct {
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
		format:          format,
		groupByPrefix:   opts.GroupByPrefix,
		prefixDelemiter: opts.PrefixDelimiter,
		path:            absolutePath,
		ch:              make(chan *entry.Entry),
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
	prefixFileMap := make(map[string]*bufio.Writer)
	prefixDbIdMap := make(map[string]int)
	files := []*os.File{}
	defer func() {
		for _, file := range files {
			file.Close()
		}
	}()
	w.createPrefixFile(prefixFileMap, prefixDbIdMap, files, w.path)
	for {
		select {
		case <-ctx.Done():
			// do nothing until w.ch is closed
		case <-ticker.C:
			for _, writer := range prefixFileMap {
				writer.Flush()
			}
		case e, ok := <-w.ch:
			if !ok {
				w.chWg.Done()
				for _, writer := range prefixFileMap {
					writer.Flush()
				}
				return
			}
			w.stat.EntryCount++
			w.writeEntry(prefixFileMap, prefixDbIdMap, files, e)
		}
	}
}

func (w *fileWriter) createPrefixFile(fileMap map[string]*bufio.Writer, prefixDbIdMap map[string]int, files []*os.File, prefix string) {
	if fileMap[prefix] != nil {
		return
	}
	file, err := os.Create(prefix)
	if err != nil {
		log.Panicf("create file failed:", err)
		return
	}
	files = append(files, file)
	writer := bufio.NewWriter(file)
	fileMap[prefix] = writer
	prefixDbIdMap[prefix] = -1
}

func (w *fileWriter) writeEntry(prefixMap map[string]*bufio.Writer, prefixDbIdMap map[string]int, files []*os.File, e *entry.Entry) {
	if e.Keys == nil {
		w.createPrefixFile(prefixMap, prefixDbIdMap, files, w.getGroupFile(""))
	}
	for _, key := range e.Keys {
		if strings.Contains(key, w.prefixDelemiter) {
			w.createPrefixFile(prefixMap, prefixDbIdMap, files, w.getGroupFile(key))
		}
	}
	newDbId := e.DbId
	switch w.format {
	case CMDWriter:
		for _, key := range e.Keys {
			if w.switchDbId(prefixDbIdMap, key, newDbId) {
				writeCMD(prefixMap, w, key, w.newSelectEntry(newDbId))
			}
			writeCMD(prefixMap, w, key, e)
		}
	case AOFWriter:
		for _, key := range e.Keys {
			if w.switchDbId(prefixDbIdMap, key, newDbId) {
				writeAOF(prefixMap, w, key, w.newSelectEntry(newDbId))
			}
			writeAOF(prefixMap, w, key, e)
		}
	case JSONWriter:
		for _, key := range e.Keys {
			if w.switchDbId(prefixDbIdMap, key, newDbId) {
				writeJSON(prefixMap, w, key, w.newSelectEntry(newDbId))
			}
			writeJSON(prefixMap, w, key, e)
		}
	}
}

func (w *fileWriter) switchDbId(prefixDbIdMap map[string]int, key string, newDbId int) bool {
	groupFile := w.getGroupFile(key)
	shouldSwitch := prefixDbIdMap[groupFile] != newDbId
	if shouldSwitch {
		prefixDbIdMap[groupFile] = newDbId
	}
	return shouldSwitch
}

func writeJSON(prefixMap map[string]*bufio.Writer, w *fileWriter, key string, e *entry.Entry) {
	// compute SerializeSize for json result
	e.Serialize()
	json, _ := json.Marshal(e)
	writer := prefixMap[w.getGroupFile(key)]
	writer.Write(json)
	writer.WriteString("\n")
}

func writeAOF(prefixMap map[string]*bufio.Writer, w *fileWriter, key string, e *entry.Entry) (int, error) {
	return prefixMap[w.getGroupFile(key)].Write(e.Serialize())
}

func writeCMD(prefixMap map[string]*bufio.Writer, w *fileWriter, key string, e *entry.Entry) (int, error) {
	return prefixMap[w.getGroupFile(key)].WriteString(strings.Join(e.Argv, " ") + "\n")
}

func (w *fileWriter) getGroupFile(key string) string {
	if w.groupByPrefix == false {
		return w.path
	}
	if !strings.Contains(key, w.prefixDelemiter) {
		return w.path
	}
	prefix := strings.Split(key, w.prefixDelemiter)[0]
	if prefix == "" {
		return w.path
	}
	return w.path + "." + prefix
}

func (w *fileWriter) newSelectEntry(newDbId int) *entry.Entry {
	return &entry.Entry{
		Argv:    []string{"select", strconv.Itoa(newDbId)},
		CmdName: "select",
	}
}
