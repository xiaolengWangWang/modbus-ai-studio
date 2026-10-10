package recorder

import "fmt"

const (
	eventTX = 1 << iota
	eventRX
	eventAnalysis
	eventDetail
	eventCodecs = eventTX | eventRX | eventAnalysis | eventDetail
)

type storedEvent struct {
	tx, rx, analysis, detail []byte
	codec                    int
}

func (s *storedEvent) fields() []*[]byte {
	return []*[]byte{&s.tx, &s.rx, &s.analysis, &s.detail}
}

func storeEvent(e Event) (storedEvent, error) {
	s := storedEvent{tx: e.TX, rx: e.RX, analysis: []byte(e.Analysis), detail: []byte(e.Detail)}
	for i, field := range s.fields() {
		data, codec, err := encodeData(*field)
		if err != nil {
			return s, err
		}
		*field = data
		if codec == codecZlib {
			s.codec |= 1 << i
		}
	}
	return s, nil
}

// Short text stays TEXT for SQL tools; compressed text uses SQLite's BLOB
// storage class. data_codec identifies the fields that require decompression.
func (s storedEvent) text(data []byte, flag int) any {
	if s.codec&flag != 0 {
		return data
	}
	return string(data)
}

func restoreEvent(e *Event, s storedEvent) error {
	if s.codec < 0 || s.codec & ^eventCodecs != 0 {
		return fmt.Errorf("未知日志压缩标记 %d", s.codec)
	}
	for i, field := range s.fields() {
		codec := codecPlain
		if s.codec&(1<<i) != 0 {
			codec = codecZlib
		}
		data, err := decodeData(*field, codec)
		if err != nil {
			return err
		}
		*field = data
	}
	e.TX, e.RX, e.Analysis, e.Detail = s.tx, s.rx, string(s.analysis), string(s.detail)
	return nil
}
