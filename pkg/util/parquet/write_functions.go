// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package parquet

import (
	"bytes"
	"reflect"
	"unsafe"

	"github.com/apache/arrow/go/v11/parquet"
	"github.com/apache/arrow/go/v11/parquet/file"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/errors"
)

// batchAlloc pre-allocates the arrays required to pass encoded datums to the
// WriteBatch method on file.ColumnChunkWriter implementations (ex.
// (*file.BooleanColumnChunkWriter) WriteBatch).
//
// This scheme works because the arrays are merely used as "carriers" to pass
// multiple encoded datums to WriteBatch. Every WriteBatch implementation
// synchronously copies values out of the array and returns without having saved
// a reference to the array for re-use.
//
// This means any array below will not be in use outside the writeBatch
// function below.
type batchAlloc struct {
	_                      util.NoCopy
	boolBatch              [1]bool
	int32Batch             [1]int32
	int64Batch             [1]int64
	byteArrayBatch         [1]parquet.ByteArray
	fixedLenByteArrayBatch [1]parquet.FixedLenByteArray
}

// The following variables are used when writing datums which are not in arrays.
//
// nonNilDefLevel represents a definition level of 1, meaning that the value is non-nil.
// nilDefLevel represents a definition level of 0, meaning that the value is nil.
// Any corresponding repetition level should be 0 as nonzero repetition levels are only valid for
// arrays in this library.
//
// For more info on definition levels, refer to
// https://arrow.apache.org/blog/2022/10/05/arrow-parquet-encoding-part-1/
var nonNilDefLevel = []int16{1}
var nilDefLevel = []int16{0}

// The following variables are used when writing datums which are in arrays. This explanation
// is valid for the array schema constructed in makeColumn.
//
// In summary:
// - def level 0 means the array is null
// - def level 1 means the array is not null, but is empty.
// - def level 2 means the array is not null, and contains a null datum
// - def level 3 means the array is not null, and contains a non-null datum
// - rep level 0 indicates the start of a new array (which may be null or non-null depending on the def level)
// - rep level 1 indicates that we are writing to an existing array
//
// Examples:
//
// Null Array
// d := tree.DNull
// writeFn(tree.DNull, ..., defLevels = [0], repLevels = [0])
//
// Empty Array
// d := tree.NewDArray(types.Int)
// d.Array = tree.Datums{}
// writeFn(tree.DNull, ..., defLevels = [1], repLevels = [0])
//
// # Multiple, Typical Arrays
//
// d := tree.NewDArray(types.Int)
// d.Array = tree.Datums{1, 2, NULL, 3, 4}
// d2 := tree.NewDArray(types.Int)
// d2.Array = tree.Datums{1, 1}
// writeFn(d.Array[0], ..., defLevels = [3], repLevels = [0]) -- repLevel 0 indicates the start of an array
// writeFn(d.Array[1], ..., defLevels = [3], repLevels = [1]) -- repLevel 1 writes the datum in the array
// writeFn(tree.DNull, ..., defLevels = [2], repLevels = [1]) -- defLevel 2 indicates a null datum
// writeFn(d.Array[3], ..., defLevels = [3], repLevels = [1])
// writeFn(d.Array[4], ..., defLevels = [3], repLevels = [1])
//
// writeFn(d2.Array[0], ..., defLevels = [3], repLevels = [0]) -- repLevel 0 indicates the start of a new array
// writeFn(d2.Array[1], ..., defLevels = [3], repLevels = [1])
//
// For more info on definition levels and repetition levels, refer to
// https://arrow.apache.org/blog/2022/10/08/arrow-parquet-encoding-part-2/
var newEntryRepLevel = []int16{0}
var arrayEntryRepLevel = []int16{1}
var nilArrayDefLevel = []int16{0}
var zeroLengthArrayDefLevel = []int16{1}
var arrayEntryNilDefLevel = []int16{2}
var arrayEntryNonNilDefLevel = []int16{3}

// A colWriter is responsible for writing a datum to a file.ColumnChunkWriter.
type colWriter interface {
	Write(d tree.Datum, w file.ColumnChunkWriter, a *batchAlloc) error
}

type scalarWriter writeFn

func (w scalarWriter) Write(d tree.Datum, cw file.ColumnChunkWriter, a *batchAlloc) error {
	return writeScalar(d, cw, a, writeFn(w))
}

func writeScalar(d tree.Datum, w file.ColumnChunkWriter, a *batchAlloc, wFn writeFn) error {
	if d == tree.DNull {
		if err := wFn(tree.DNull, w, a, nilDefLevel, newEntryRepLevel); err != nil {
			return err
		}
		return nil
	}

	if err := wFn(d, w, a, nonNilDefLevel, newEntryRepLevel); err != nil {
		return err
	}
	return nil
}

type arrayWriter writeFn

func (w arrayWriter) Write(d tree.Datum, cw file.ColumnChunkWriter, a *batchAlloc) error {
	return writeArray(d, cw, a, writeFn(w))
}

func writeArray(d tree.Datum, w file.ColumnChunkWriter, a *batchAlloc, wFn writeFn) error {
	if d == tree.DNull {
		return wFn(tree.DNull, w, a, nilArrayDefLevel, newEntryRepLevel)
	}
	di, ok := tree.AsDArray(d)
	if !ok {
		return pgerror.Newf(pgcode.DatatypeMismatch, "expected DArray, found %T", d)
	}
	if len(di.Array) == 0 {
		return wFn(tree.DNull, w, a, zeroLengthArrayDefLevel, newEntryRepLevel)
	}

	repLevel := newEntryRepLevel
	for i, childDatum := range di.Array {
		if i == 1 {
			repLevel = arrayEntryRepLevel
		}
		if childDatum == tree.DNull {
			if err := wFn(childDatum, w, a, arrayEntryNilDefLevel, repLevel); err != nil {
				return err
			}
		} else {
			if err := wFn(childDatum, w, a, arrayEntryNonNilDefLevel, repLevel); err != nil {
				return err
			}
		}
	}
	return nil
}

// A writeFn encodes a datum and writes it using the provided column chunk writer.
// The caller is responsible for ensuring that the def levels and rep levels are correct.
type writeFn func(d tree.Datum, w file.ColumnChunkWriter, a *batchAlloc, defLevels, repLevels []int16) error

func writeInt32(
	d tree.Datum, w file.ColumnChunkWriter, a *batchAlloc, defLevels, repLevels []int16,
) error {
	if d == tree.DNull {
		return writeBatch[int32](w, a.int32Batch[:], defLevels, repLevels)
	}
	di, ok := tree.AsDInt(d)
	if !ok {
		return pgerror.Newf(pgcode.DatatypeMismatch, "expected DInt, found %T", d)
	}
	a.int32Batch[0] = int32(di)
	return writeBatch[int32](w, a.int32Batch[:], defLevels, repLevels)
}

func writeInt64(
	d tree.Datum, w file.ColumnChunkWriter, a *batchAlloc, defLevels, repLevels []int16,
) error {
	if d == tree.DNull {
		return writeBatch[int64](w, a.int64Batch[:], defLevels, repLevels)
	}
	di, ok := tree.AsDInt(d)
	if !ok {
		return pgerror.Newf(pgcode.DatatypeMismatch, "expected DInt, found %T", d)
	}
	a.int64Batch[0] = int64(di)
	return writeBatch[int64](w, a.int64Batch[:], defLevels, repLevels)
}

func writeBool(
	d tree.Datum, w file.ColumnChunkWriter, a *batchAlloc, defLevels, repLevels []int16,
) error {
	if d == tree.DNull {
		return writeBatch[bool](w, a.boolBatch[:], defLevels, repLevels)
	}
	di, ok := tree.AsDBool(d)
	if !ok {
		return pgerror.Newf(pgcode.DatatypeMismatch, "expected DBool, found %T", d)
	}
	a.boolBatch[0] = bool(di)
	return writeBatch[bool](w, a.boolBatch[:], defLevels, repLevels)
}

func writeString(
	d tree.Datum, w file.ColumnChunkWriter, a *batchAlloc, defLevels, repLevels []int16,
) error {
	if d == tree.DNull {
		return writeBatch[parquet.ByteArray](w, a.byteArrayBatch[:], defLevels, repLevels)
	}
	di, ok := tree.AsDString(d)
	if !ok {
		return pgerror.Newf(pgcode.DatatypeMismatch, "expected DString, found %T", d)
	}
	var b parquet.ByteArray
	b, err := unsafeGetBytes(string(di))
	if err != nil {
		return err
	}
	a.byteArrayBatch[0] = b
	return writeBatch[parquet.ByteArray](w, a.byteArrayBatch[:], defLevels, repLevels)
}

// unsafeGetBytes returns []byte in the underlying string,
// without incurring copy.
// This unsafe mechanism is safe to use here because the returned bytes will
// be copied by the parquet library when writing a datum to a column chunk.
// See https://groups.google.com/g/golang-nuts/c/Zsfk-VMd_fU/m/O1ru4fO-BgAJ
//
// TODO(jayant): once we upgrade to Go 1.20, we can replace this with a less unsafe
// implementation. See https://www.sobyte.net/post/2022-09/string-byte-convertion/
func unsafeGetBytes(s string) ([]byte, error) {
	// For an empty string, the code below will return a []byte(nil) instead of a
	// []byte{}. Using the former will result in parquet readers decoding the
	// binary data into	[1]byte{'\x00'}, which is incorrect because it
	// represents a string of length 1 instead of 0.
	if len(s) == 0 {
		return []byte{}, nil
	}
	const maxStrLen = 1 << 30
	if len(s) > maxStrLen {
		return nil, bytes.ErrTooLarge
	}
	if len(s) == 0 {
		return nil, nil
	}
	p := unsafe.Pointer((*reflect.StringHeader)(unsafe.Pointer(&s)).Data)
	return (*[maxStrLen]byte)(p)[:len(s):len(s)], nil
}

func writeTimestamp(
	d tree.Datum, w file.ColumnChunkWriter, a *batchAlloc, defLevels, repLevels []int16,
) error {
	if d == tree.DNull {
		return writeBatch[parquet.ByteArray](w, a.byteArrayBatch[:], defLevels, repLevels)
	}

	_, ok := tree.AsDTimestamp(d)
	if !ok {
		return pgerror.Newf(pgcode.DatatypeMismatch, "expected DTimestamp, found %T", d)
	}

	fmtCtx := tree.NewFmtCtx(tree.FmtBareStrings)
	d.Format(fmtCtx)

	a.byteArrayBatch[0] = parquet.ByteArray(fmtCtx.CloseAndGetString())
	return writeBatch[parquet.ByteArray](w, a.byteArrayBatch[:], defLevels, repLevels)
}

func writeUUID(
	d tree.Datum, w file.ColumnChunkWriter, a *batchAlloc, defLevels, repLevels []int16,
) error {
	if d == tree.DNull {
		return writeBatch[parquet.FixedLenByteArray](w, a.fixedLenByteArrayBatch[:], defLevels, repLevels)
	}

	di, ok := tree.AsDUuid(d)
	if !ok {
		return pgerror.Newf(pgcode.DatatypeMismatch, "expected DUuid, found %T", d)
	}
	a.fixedLenByteArrayBatch[0] = di.UUID.GetBytes()
	return writeBatch[parquet.FixedLenByteArray](w, a.fixedLenByteArrayBatch[:], defLevels, repLevels)
}

func writeDecimal(
	d tree.Datum, w file.ColumnChunkWriter, a *batchAlloc, defLevels, repLevels []int16,
) error {
	if d == tree.DNull {
		return writeBatch[parquet.ByteArray](w, a.byteArrayBatch[:], defLevels, repLevels)
	}
	di, ok := tree.AsDDecimal(d)
	if !ok {
		return pgerror.Newf(pgcode.DatatypeMismatch, "expected DDecimal, found %T", d)
	}
	a.byteArrayBatch[0] = parquet.ByteArray(di.String())
	return writeBatch[parquet.ByteArray](w, a.byteArrayBatch[:], defLevels, repLevels)
}

// parquetDatatypes are the physical types used in the parquet library.
type parquetDatatypes interface {
	bool | int32 | int64 | parquet.ByteArray | parquet.FixedLenByteArray
}

// batchWriter is an interface representing parquet column chunk writers such as
// file.Int64ColumnChunkWriter and file.BooleanColumnChunkWriter.
type batchWriter[T parquetDatatypes] interface {
	WriteBatch(values []T, defLevels, repLevels []int16) (valueOffset int64, err error)
}

func writeBatch[T parquetDatatypes](
	w file.ColumnChunkWriter, batchAlloc []T, defLevels, repLevels []int16,
) (err error) {
	bw, ok := w.(batchWriter[T])
	if !ok {
		return errors.AssertionFailedf("expected batchWriter of type %T, but found %T instead", []T(nil), w)
	}
	_, err = bw.WriteBatch(batchAlloc, defLevels, repLevels)
	return err
}