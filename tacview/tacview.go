package tacview

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spkg/bom"
)

var bomHeader = []byte{0xef, 0xbb, 0xbf}
var objectLineRe = regexp.MustCompile(`^(-?[0-9a-fA-F]+)(?:,((?:.|\n)*)+)?`)
var keyRe = regexp.MustCompilePOSIX("^(.*)=(.*)$")

func splitPropertyTokens(s string) (tokens []string, err error) {
	var runes []rune
	inEscape := false
	for _, r := range s {
		switch {
		case inEscape:
			inEscape = false
			fallthrough
		default:
			runes = append(runes, r)
		case r == '\\':
			inEscape = true
		case r == ',':
			tokens = append(tokens, string(runes))
			runes = runes[:0]
		}
	}
	tokens = append(tokens, string(runes))
	if inEscape {
		err = errors.New("invalid escape")
	}
	return tokens, err
}

type Header struct {
	FileType         string
	FileVersion      string
	ReferenceTime    time.Time
	InitialTimeFrame TimeFrame
}

type Reader struct {
	Header Header
	reader *bufio.Reader
}

type Writer struct {
	writer *bufio.Writer
	closer io.Closer
}

type TimeFrame struct {
	Offset  float64
	Objects []*Object
}

type Property struct {
	Key   string
	Value string
}

type Object struct {
	Id         uint64
	Properties []*Property
	Deleted    bool
}

func NewTimeFrame() *TimeFrame {
	return &TimeFrame{
		Objects: make([]*Object, 0),
	}
}

func NewWriter(writer io.WriteCloser, header *Header) (*Writer, error) {
	w := &Writer{
		writer: bufio.NewWriter(writer),
		closer: writer,
	}
	return w, w.writeHeader(header)
}

func NewReader(reader io.Reader) (*Reader, error) {
	r := &Reader{reader: bufio.NewReader(bom.NewReader(reader))}
	err := r.readHeader()
	return r, err
}

func (w *Writer) Close() error {
	err := w.writer.Flush()
	if err != nil {
		return err
	}
	return w.closer.Close()
}

func (w *Writer) writeHeader(header *Header) error {
	_, err := w.writer.Write(bomHeader)
	if err != nil {
		return err
	}

	return header.Write(w.writer)
}

func (w *Writer) WriteTimeFrame(tf *TimeFrame) error {
	return tf.Write(w.writer, true)
}

func (h *Header) Write(writer *bufio.Writer) error {
	_, err := writer.WriteString("FileType=text/acmi/tacview\nFileVersion=2.2\n")
	if err != nil {
		return err
	}

	h.InitialTimeFrame.Write(writer, false)

	return writer.Flush()
}

func (tf *TimeFrame) Get(id uint64) *Object {
	for _, object := range tf.Objects {
		if object.Id == id {
			return object
		}
	}
	return nil
}

func (tf *TimeFrame) Write(writer *bufio.Writer, includeOffset bool) error {
	if includeOffset {
		_, err := writer.WriteString(fmt.Sprintf("#%F\n", tf.Offset))
		if err != nil {
			return err
		}
	}

	for _, object := range tf.Objects {
		object.Write(writer)
	}

	return nil
}

func (o *Object) Set(key string, value string) {
	for _, property := range o.Properties {
		if property.Key == key {
			property.Value = value
			return
		}
	}
	o.Properties = append(o.Properties, &Property{Key: key, Value: value})
}

func (o *Object) Get(key string) *Property {
	for _, property := range o.Properties {
		if property.Key == key {
			return property
		}
	}
	return nil
}

func (o *Object) Write(writer *bufio.Writer) error {
	if o.Deleted {
		_, err := writer.WriteString(fmt.Sprintf("-%x\n", o.Id))
		return err
	}

	_, err := writer.WriteString(fmt.Sprintf("%x", o.Id))
	if err != nil {
		return err
	}

	if len(o.Properties) == 0 {
		_, err = writer.WriteString(",\n")
		return err
	}

	for _, property := range o.Properties {
		_, err = writer.WriteString(fmt.Sprintf(
			",%s=%s",
			property.Key,
			strings.Replace(strings.Replace(property.Value, "\n", "\\\n", -1), ",", "\\,", -1)),
		)
		if err != nil {
			return err
		}
	}

	_, err = writer.WriteString("\n")
	return err
}

func (r *Reader) parseObject(object *Object, data string) error {
	parts, err := splitPropertyTokens(data)
	if err != nil {
		return err
	}

	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		partSplit := strings.SplitN(part, "=", 2)
		if len(partSplit) != 2 {
			return fmt.Errorf("Failed to parse property: `%v`", part)
		}

		object.Properties = append(object.Properties, &Property{Key: partSplit[0], Value: partSplit[1]})
	}

	return nil
}

func (r *Reader) ProcessTimeFrames(processes int, timeFrame chan<- *TimeFrame) error {
	bufferChan := make(chan []byte)

	var wg sync.WaitGroup
	for i := 0; i < processes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for {
				data, ok := <-bufferChan
				if data == nil || !ok {
					return
				}

				tf := NewTimeFrame()
				err := r.parseTimeFrame(data, tf, true)
				if err != nil && err != io.EOF {
					fmt.Printf("Failed to process time frame: (%v) %v\n", string(data), err)
					close(timeFrame)
					return
				}

				timeFrame <- tf
			}
		}()
	}

	err := r.timeFrameProducer(bufferChan)
	for i := 0; i < processes; i++ {
		bufferChan <- nil
	}

	wg.Wait()
	close(timeFrame)
	return err
}

func (r *Reader) timeFrameProducer(buffs chan<- []byte) error {
	var buf []byte
	for {
		line, err := r.reader.ReadBytes('\n')
		if err == io.EOF {
			buffs <- buf
			return nil
		} else if err != nil {
			return err
		}

		if line[0] != '#' {
			buf = append(buf, line...)
			continue
		}

		if len(buf) > 0 {
			buffs <- buf
		}
		buf = line
	}
}

func (r *Reader) parseTimeFrame(data []byte, timeFrame *TimeFrame, parseOffset bool) error {
	reader := bufio.NewReader(bytes.NewBuffer(data))
	return r.readTimeFrame(reader, timeFrame, parseOffset)
}

func (r *Reader) readTimeFrame(reader *bufio.Reader, timeFrame *TimeFrame, parseOffset bool) error {
	if parseOffset {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}

		if len(line) == 0 || line[0] != '#' {
			return fmt.Errorf("Expected time frame offset, found `%v`", line)
		}

		offset, err := strconv.ParseFloat(line[1:len(line)-1], 64)
		if err != nil {
			return err
		}

		timeFrame.Offset = offset
	}

	timeFrameObjectCache := make(map[uint64]*Object)

	for {
		buffer := ""

		nextLinePrefix, err := reader.Peek(1)
		if err != nil {
			return err
		}

		if nextLinePrefix[0] == '#' {
			break
		}

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return err
			}

			buffer = buffer + strings.TrimSuffix(line, "\n")
			if !strings.HasSuffix(buffer, "\\") {
				break
			}

			buffer = buffer[:len(buffer)-1] + "\n"
		}

		rawLineParts := objectLineRe.FindAllStringSubmatch(buffer, -1)
		if len(rawLineParts) != 1 {
			return fmt.Errorf("Failed to parse line: `%v` (%v)", buffer, len(rawLineParts))
		}

		lineParts := rawLineParts[0]

		if lineParts[1][0] == '-' {
			objectId, err := strconv.ParseUint(lineParts[1][1:], 16, 64)
			if err != nil {
				return err
			}

			if timeFrameObjectCache[objectId] != nil {
				timeFrameObjectCache[objectId].Deleted = true
			} else {
				object := &Object{Id: objectId, Properties: make([]*Property, 0), Deleted: true}
				timeFrameObjectCache[objectId] = object
				timeFrame.Objects = append(timeFrame.Objects, object)
			}
		} else {
			objectId, err := strconv.ParseUint(lineParts[1], 16, 64)
			if err != nil {
				return err
			}
			object, ok := timeFrameObjectCache[objectId]
			if !ok {
				object = &Object{
					Id:         objectId,
					Properties: make([]*Property, 0),
				}
				timeFrameObjectCache[objectId] = object
				timeFrame.Objects = append(timeFrame.Objects, object)
			}

			err = r.parseObject(object, lineParts[2])
			if err != nil {
				return err
			}
		}

	}

	return nil
}

func (r *Reader) readHeader() error {
	foundFileType := false
	foundFileVersion := false

	for {
		line, err := r.reader.ReadString('\n')
		if err != nil {
			return err
		}

		line = strings.TrimSuffix(line, "\n")

		matches := keyRe.FindAllStringSubmatch(line, -1)
		if len(matches) != 1 {
			return fmt.Errorf("Failed to parse key pair from line: `%v`", line)
		}

		if matches[0][1] == "FileType" && !foundFileType {
			foundFileType = true
			r.Header.FileType = matches[0][1]
		} else if matches[0][1] == "FileVersion" && !foundFileVersion {
			foundFileVersion = true
			r.Header.FileVersion = matches[0][2]
		}

		if foundFileType && foundFileVersion {
			break
		}
	}

	r.Header.InitialTimeFrame = *NewTimeFrame()
	err := r.readTimeFrame(r.reader, &r.Header.InitialTimeFrame, false)
	if err != nil {
		return err
	}

	globalObj := r.Header.InitialTimeFrame.Get(0)
	if globalObj == nil {
		return fmt.Errorf("No global object found in initial time frame")
	}

	referenceTimeProperty := globalObj.Get("ReferenceTime")
	if referenceTimeProperty == nil {
		return fmt.Errorf("Global object is missing ReferenceTime")
	}

	referenceTime, err := time.Parse("2006-01-02T15:04:05Z", referenceTimeProperty.Value)
	if err != nil {
		return fmt.Errorf("Failed to parse ReferenceTime: `%v`", referenceTimeProperty.Value)
	}

	r.Header.ReferenceTime = referenceTime

	return nil
}
