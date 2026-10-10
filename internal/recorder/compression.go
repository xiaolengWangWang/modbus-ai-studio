package recorder

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"sync"
)

const (
	codecPlain = 0
	codecZlib  = 1

	compressionThreshold = 256
	maxDecodedData       = 64 << 20
)

// Reuse the deflate working memory without retaining a caller's payload.
var dataWriters = sync.Pool{New: func() any {
	w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
	return w
}}

type dataReader struct {
	input  bytes.Reader
	reader io.ReadCloser
}

// Keeping the input reader separate lets the pool release the compressed BLOB
// while retaining the inflater's working memory for the next queried row.
var dataReaders sync.Pool

// encodeData compresses larger fields only when it saves space. Plain values
// keep the legacy representation, including the distinction between nil and empty.
func encodeData(data []byte) ([]byte, int, error) {
	if len(data) < compressionThreshold || len(data) > maxDecodedData {
		return data, codecPlain, nil
	}
	var out bytes.Buffer
	w := dataWriters.Get().(*zlib.Writer)
	w.Reset(&out)
	defer func() {
		w.Reset(io.Discard)
		dataWriters.Put(w)
	}()
	if _, err := w.Write(data); err != nil {
		return nil, codecPlain, fmt.Errorf("压缩记录数据失败：%w", err)
	}
	if err := w.Close(); err != nil {
		return nil, codecPlain, fmt.Errorf("压缩记录数据失败：%w", err)
	}
	if out.Len() >= len(data) {
		return data, codecPlain, nil
	}
	return out.Bytes(), codecZlib, nil
}

// decodeData reads both existing plain rows and compressed rows. The limit is
// applied only to zlib expansion so large legacy rows remain readable.
func decodeData(data []byte, codec int) ([]byte, error) {
	return decodeDataLimit(data, codec, maxDecodedData)
}

func decodeDataLimit(data []byte, codec int, limit int64) ([]byte, error) {
	switch codec {
	case codecPlain:
		return data, nil
	case codecZlib:
	default:
		return nil, fmt.Errorf("不支持的记录压缩编码 %d", codec)
	}
	d, _ := dataReaders.Get().(*dataReader)
	if d == nil {
		d = new(dataReader)
	}
	d.input.Reset(data)
	var err error
	if d.reader == nil {
		d.reader, err = zlib.NewReader(&d.input)
	} else {
		err = d.reader.(zlib.Resetter).Reset(&d.input, nil)
	}
	if err != nil {
		d.input.Reset(nil)
		if d.reader != nil {
			d.reader.Close()
		}
		return nil, fmt.Errorf("解压记录数据失败：%w", err)
	}
	defer func() {
		d.reader.Close()
		d.input.Reset(nil)
		dataReaders.Put(d)
	}()
	decoded, err := io.ReadAll(io.LimitReader(d.reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("解压记录数据失败：%w", err)
	}
	if int64(len(decoded)) > limit {
		return nil, fmt.Errorf("记录解压后超过 %d 字节上限", limit)
	}
	return decoded, nil
}
