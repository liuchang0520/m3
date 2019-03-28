// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package proto

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/m3db/m3x/checked"

	dpb "github.com/golang/protobuf/protoc-gen-go/descriptor"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/m3db/m3/src/dbnode/encoding"
	"github.com/m3db/m3/src/dbnode/encoding/m3tsz"
	"github.com/m3db/m3/src/dbnode/ts"
	xtime "github.com/m3db/m3x/time"
)

const (
	// Maximum capacity of a checked.Bytes that will be retained between resets.
	maxCapacityUnmarshalBufferRetain = 1024
)

var (
	itErrPrefix                 = "proto iterator:"
	errIteratorReaderIsRequired = fmt.Errorf("%s reader is required", itErrPrefix)
	errIteratorSchemaIsRequired = fmt.Errorf("%s schema is required", itErrPrefix)
)

type iterator struct {
	opts                   encoding.Options
	err                    error
	schema                 *desc.MessageDescriptor
	stream                 encoding.IStream
	consumedFirstMessage   bool
	lastIterated           *dynamic.Message
	lastIteratedAnnotation []byte
	byteFieldDictLRUSize   int
	// TODO(rartoul): Update these as we traverse the stream if we encounter
	// a mid-stream schema change: https://github.com/m3db/m3/issues/1471
	customFields []customFieldState

	// Fields that are reused between function calls to
	// avoid allocations.
	varIntBuf         [8]byte
	bitsetValues      []int
	unmarshalProtoBuf checked.Bytes

	done bool
	// Can i just reuse done for this?
	closed bool

	m3tszIterator *m3tsz.ReaderIterator
}

// NewIterator creates a new iterator.
func NewIterator(
	reader io.Reader,
	schema *desc.MessageDescriptor,
	opts encoding.Options,
) encoding.ReaderIterator {
	stream := encoding.NewIStream(reader)

	var currCustomFields []customFieldState
	if schema != nil {
		currCustomFields = customFields(nil, schema)
	}
	return &iterator{
		opts:         opts,
		schema:       schema,
		stream:       stream,
		lastIterated: dynamic.NewMessage(schema),
		customFields: currCustomFields,

		m3tszIterator: m3tsz.NewReaderIterator(nil, stream, false, opts).(*m3tsz.ReaderIterator),
	}
}

func (it *iterator) Next() bool {
	if it.schema == nil {
		it.err = errIteratorSchemaIsRequired
	}

	if !it.hasNext() {
		return false
	}

	if !it.consumedFirstMessage {
		if err := it.readHeader(); err != nil {
			it.err = err
			return false
		}
	}

	moreDataControlBit, err := it.stream.ReadBit()
	if err == io.EOF || (err == nil && moreDataControlBit == opCodeNoMoreData) {
		it.done = true
		return false
	}
	if err != nil {
		it.err = err
		return false
	}

	it.m3tszIterator.ReadTimestamp()
	if it.m3tszIterator.Err() != nil {
		it.err = it.m3tszIterator.Err()
		return false
	}

	if err := it.readCustomValues(); err != nil {
		it.err = err
		return false
	}

	if err := it.readProtoValues(); err != nil {
		it.err = err
		return false
	}

	// TODO(rartoul): Add MarshalInto method to ProtoReflect library to save
	// allocations: https://github.com/m3db/m3/issues/1471
	// Keep the annotation version of the last iterated protobuf message up to
	// date so we can return it in subsequent calls to Current(), otherwise we'd
	// have to marshal it in the Current() call where we can't handle errors.
	it.lastIteratedAnnotation, err = it.lastIterated.Marshal()
	if err != nil {
		it.err = fmt.Errorf(
			"%s: error marshaling last iterated proto message: %v", itErrPrefix, err)
		return false
	}

	it.consumedFirstMessage = true
	return it.hasNext()
}

func (it *iterator) Current() (ts.Datapoint, xtime.Unit, ts.Annotation) {
	dp, unit, _ := it.m3tszIterator.Current()
	return dp, unit, it.lastIteratedAnnotation
}

func (it *iterator) Err() error {
	return it.err
}

func (it *iterator) Reset(reader io.Reader) {
	it.stream.Reset(reader)
	it.m3tszIterator.Reset(reader)

	it.err = nil
	it.consumedFirstMessage = false
	it.lastIterated = dynamic.NewMessage(it.schema)
	it.lastIteratedAnnotation = nil
	it.customFields = resetCustomFields(it.customFields, it.schema)
	it.done = false
	it.closed = false
	it.byteFieldDictLRUSize = 0
}

// SetSchema sets the encoders schema.
func (it *iterator) SetSchema(schema *desc.MessageDescriptor) {
	it.schema = schema
	it.customFields = resetCustomFields(it.customFields, it.schema)
}

func (it *iterator) Close() {
	if it.closed {
		return
	}

	it.closed = true
	it.Reset(nil)
	it.stream.Reset(nil)
	it.m3tszIterator.Reset(nil)

	if it.unmarshalProtoBuf != nil && it.unmarshalProtoBuf.Cap() > maxCapacityUnmarshalBufferRetain {
		// Only finalize the buffer if its grown too large to prevent pooled
		// iterators from growing excessively large.
		it.unmarshalProtoBuf.DecRef()
		it.unmarshalProtoBuf.Finalize()
		it.unmarshalProtoBuf = nil
	}

	if pool := it.opts.ReaderIteratorPool(); pool != nil {
		pool.Put(it)
	}
}

func (it *iterator) readHeader() error {
	// Can ignore the version number for now because we only have one.
	_, err := it.readVarInt()
	if err != nil {
		return err
	}

	byteFieldDictLRUSize, err := it.readVarInt()
	if err != nil {
		return err
	}

	it.byteFieldDictLRUSize = int(byteFieldDictLRUSize)
	return it.readCustomFieldsSchema()
}

func (it *iterator) readCustomFieldsSchema() error {
	maxCustomFieldNum, err := it.readVarInt()
	if err != nil {
		return err
	}

	if maxCustomFieldNum > maxCustomFieldNum {
		return fmt.Errorf(
			"%s maximum custom field in header is %d but maximum allowed is %d",
			itErrPrefix, maxCustomFieldNum, maxCustomFieldNum)
	}

	if maxCustomFieldNum <= maxTSZFieldsCapacityRetain && it.customFields != nil {
		it.customFields = it.customFields[:0]
	} else {
		it.customFields = make([]customFieldState, 0, maxCustomFieldNum)
	}

	for i := 1; i <= int(maxCustomFieldNum); i++ {
		fieldTypeBits, err := it.stream.ReadBits(3)
		if err != nil {
			return err
		}

		fieldType := customFieldType(fieldTypeBits)
		if fieldType == cNotCustomEncoded {
			continue
		}

		it.customFields = append(it.customFields, newCustomFieldState(i, fieldType))
	}

	return nil
}

func (it *iterator) readCustomValues() error {
	var err error

	if !it.consumedFirstMessage {
		err = it.readFirstCustomValues()
	} else {
		err = it.readNextCustomValues()
	}

	return err
}

func (it *iterator) readProtoValues() error {
	protoChangesControlBit, err := it.stream.ReadBit()
	if err != nil {
		return fmt.Errorf("%s err reading proto changes control bit: %v", itErrPrefix, err)
	}

	if protoChangesControlBit == opCodeNoChange {
		// No changes since previous message.
		return nil
	}

	fieldsSetToDefaultControlBit, err := it.stream.ReadBit()
	if err != nil {
		return fmt.Errorf("%s err reading field set to default control bit: %v", itErrPrefix, err)
	}

	if fieldsSetToDefaultControlBit == opCodeFieldsSetToDefaultProtoMarshal {
		// Some fields set to default value, need to read bitset.
		err = it.readBitset()
		if err != nil {
			return fmt.Errorf(
				"error readining changed proto field numbers bitset: %v", err)
		}
	}

	marshalLen, err := it.readVarInt()
	if err != nil {
		return fmt.Errorf("%s err reading proto length varint: %v", itErrPrefix, err)
	}

	if marshalLen > maxMarshaledProtoMessageSize {
		return fmt.Errorf(
			"%s marshaled protobuf size was %d which is larger than the maximum of %d",
			itErrPrefix, marshalLen, maxMarshaledProtoMessageSize)
	}

	it.resetUnmarshalProtoBuffer(int(marshalLen))
	unmarshalBytes := it.unmarshalProtoBuf.Bytes()
	n, err := it.stream.Read(unmarshalBytes)
	if err != nil {
		return fmt.Errorf("%s: error reading marshaled proto bytes: %v", itErrPrefix, err)
	}
	if n != int(marshalLen) {
		return fmt.Errorf(
			"%s tried to read %d marshaled proto bytes but only read %d",
			itErrPrefix, int(marshalLen), n)
	}

	m := dynamic.NewMessage(it.schema)
	err = m.Unmarshal(unmarshalBytes)
	if err != nil {
		return fmt.Errorf("error unmarshaling protobuf: %v", err)
	}

	for _, field := range m.GetKnownFields() {
		var (
			messageType = field.GetMessageType()
			fieldNumInt = int(field.GetNumber())
		)
		if messageType == nil && !field.IsRepeated() {
			continue
		}

		curVal := m.GetFieldByNumber(fieldNumInt)
		isDefaultValue, err := isDefaultValue(field, curVal)
		if err != nil {
			return fmt.Errorf(
				"%s error: %v checking if %v is default value for field %s",
				itErrPrefix, err, curVal, field.String())
		}

		if isDefaultValue {
			// The value may appear as a default value simply because it hasn't changed
			// since the last message. Ignore for now and if it truly changed to become
			// a default value it will get handled when we loop through the bitset later.
			continue
		}

		// If the unmarshaled value is not the default value for the field then
		// we know it has changed and needs to be updated.
		it.lastIterated.SetFieldByNumber(fieldNumInt, curVal)
	}

	if fieldsSetToDefaultControlBit == 1 {
		for _, fieldNum := range it.bitsetValues {
			err := it.lastIterated.TryClearFieldByNumber(fieldNum)
			if err != nil {
				return fmt.Errorf(
					"%s: error clearing field number: %d, err: %v",
					itErrPrefix, fieldNum, err)
			}
		}
	}

	return nil
}

func (it *iterator) readFirstCustomValues() error {
	for i, customField := range it.customFields {
		switch {
		case isCustomFloatEncodedField(customField.fieldType):
			if err := it.readFirstTSZValue(i, customField); err != nil {
				return err
			}
		case customField.fieldType == cBytes:
			if err := it.readBytesValue(i, customField); err != nil {
				return err
			}
		case isCustomIntEncodedField(customField.fieldType):
			if err := it.readIntValue(i, customField, true); err != nil {
				return err
			}
		default:
			return fmt.Errorf(
				"%s: unhandled custom field type: %v", itErrPrefix, customField.fieldType)
		}
	}

	return nil
}

func (it *iterator) readFirstTSZValue(i int, customField customFieldState) error {
	fb, xor, err := it.readFullFloatVal()
	if err != nil {
		return err
	}

	it.customFields[i].prevFloatBits = fb
	it.customFields[i].prevXOR = xor
	if err := it.updateLastIteratedWithCustomValues(i); err != nil {
		return err
	}

	return nil
}

func (it *iterator) readNextCustomValues() error {
	for i, customField := range it.customFields {
		switch {
		case isCustomFloatEncodedField(customField.fieldType):
			if err := it.readNextTSZValue(i, customField); err != nil {
				return err
			}

		case customField.fieldType == cBytes:
			if err := it.readBytesValue(i, customField); err != nil {
				return err
			}

		case isCustomIntEncodedField(customField.fieldType):
			if err := it.readIntValue(i, customField, false); err != nil {
				return err
			}

		default:
			return fmt.Errorf(
				"%s: unknown custom field type: %v", itErrPrefix, customField.fieldType)
		}
	}

	return nil
}

func (it *iterator) readNextTSZValue(i int, customField customFieldState) error {
	fb, xor, err := it.readFloatXOR(i)
	if err != nil {
		return err
	}

	it.customFields[i].prevFloatBits = fb
	it.customFields[i].prevXOR = xor
	if err := it.updateLastIteratedWithCustomValues(i); err != nil {
		return err
	}

	return nil
}

func (it *iterator) readIntValue(i int, customField customFieldState, first bool) error {
	if !first {
		changeExistsControlBit, err := it.stream.ReadBit()
		if err != nil {
			return fmt.Errorf(
				"%s: error trying to read int change exists control bit: %v",
				itErrPrefix, err)
		}

		if changeExistsControlBit == opCodeNoChange {
			// No change.
			return nil
		}
	}

	if err := it.readIntSig(i); err != nil {
		return fmt.Errorf(
			"%s error trying to read number of significant digits: %v",
			itErrPrefix, err)
	}

	if err := it.readIntValDiff(i); err != nil {
		return fmt.Errorf(
			"%s error trying to read int diff: %v",
			itErrPrefix, err)
	}

	if err := it.updateLastIteratedWithCustomValues(i); err != nil {
		return fmt.Errorf(
			"%s error updating last iterated with int value: %v",
			itErrPrefix, err)
	}
	return nil
}

func (it *iterator) readBytesValue(i int, customField customFieldState) error {
	bytesChangedControlBit, err := it.stream.ReadBit()
	if err != nil {
		return fmt.Errorf(
			"%s: error trying to read bytes changed control bit: %v",
			itErrPrefix, err)
	}

	if bytesChangedControlBit == opCodeNoChange {
		// No changes to the bytes value.
		return nil
	}

	// Bytes have changed since the previous value.
	valueInDictControlBit, err := it.stream.ReadBit()
	if err != nil {
		return fmt.Errorf(
			"%s error trying to read bytes changed control bit: %v",
			itErrPrefix, err)
	}
	if valueInDictControlBit == opCodeInterpretSubsequentBitsAsLRUIndex {
		dictIdxBits, err := it.stream.ReadBits(
			numBitsRequiredForNumUpToN(it.byteFieldDictLRUSize))
		if err != nil {
			return fmt.Errorf(
				"%s error trying to read bytes dict idx: %v",
				itErrPrefix, err)
		}

		dictIdx := int(dictIdxBits)
		if dictIdx >= len(customField.iteratorBytesFieldDict) || dictIdx < 0 {
			return fmt.Errorf(
				"%s read bytes field dictionary index: %d, but dictionary is size: %d",
				itErrPrefix, dictIdx, len(customField.iteratorBytesFieldDict))
		}

		bytesVal := customField.iteratorBytesFieldDict[dictIdx]
		if it.schema.FindFieldByNumber(int32(customField.fieldNum)).GetType() == dpb.FieldDescriptorProto_TYPE_STRING {
			it.lastIterated.SetFieldByNumber(customField.fieldNum, string(bytesVal))
		} else {
			it.lastIterated.SetFieldByNumber(customField.fieldNum, bytesVal)
		}

		it.moveToEndOfBytesDict(i, dictIdx)
		return nil
	}

	// New value that was not in the dict already.
	bytesLen, err := it.readVarInt()
	if err != nil {
		return fmt.Errorf(
			"%s error trying to read bytes length: %v", itErrPrefix, err)
	}

	if err := it.skipToNextByte(); err != nil {
		return fmt.Errorf(
			"%s error trying to skip bytes value bit padding: %v",
			itErrPrefix, err)
	}

	buf := make([]byte, 0, bytesLen)
	for j := 0; j < int(bytesLen); j++ {
		b, err := it.stream.ReadByte()
		if err != nil {
			return fmt.Errorf(
				"%s error trying to read byte in readBytes: %v",
				itErrPrefix, err)
		}
		buf = append(buf, b)
	}

	// TODO(rartoul): Could make this more efficient with unsafe string conversion or by pre-processing
	// schemas to only have bytes since its all the same over the wire.
	// https://github.com/m3db/m3/issues/1471
	schemaFieldType := it.schema.FindFieldByNumber(int32(customField.fieldNum)).GetType()
	if schemaFieldType == dpb.FieldDescriptorProto_TYPE_STRING {
		it.lastIterated.TrySetFieldByNumber(customField.fieldNum, string(buf))
	} else {
		it.lastIterated.TrySetFieldByNumber(customField.fieldNum, buf)
	}
	if err != nil {
		return fmt.Errorf(
			"%s error trying to set field number: %d, err: %v",
			itErrPrefix, customField.fieldNum, err)
	}

	it.addToBytesDict(i, buf)
	return nil
}

// updateLastIteratedWithCustomValues updates lastIterated with the current
// value of the custom field in it.customFields at index i. This ensures that
// when we return it.lastIterated in the call to Current() that all the
// most recent values are present.
func (it *iterator) updateLastIteratedWithCustomValues(i int) error {
	if it.lastIterated == nil {
		it.lastIterated = dynamic.NewMessage(it.schema)
	}

	var (
		fieldNum  = it.customFields[i].fieldNum
		fieldType = it.customFields[i].fieldType
	)

	switch {
	case isCustomFloatEncodedField(fieldType):
		var (
			val = math.Float64frombits(it.customFields[i].prevFloatBits)
			err error
		)
		if fieldType == cFloat64 {
			err = it.lastIterated.TrySetFieldByNumber(fieldNum, val)
		} else {
			err = it.lastIterated.TrySetFieldByNumber(fieldNum, float32(val))
		}
		return err

	case isCustomIntEncodedField(fieldType):
		switch fieldType {
		case cSignedInt64:
			val := int64(it.customFields[i].prevFloatBits)
			return it.lastIterated.TrySetFieldByNumber(fieldNum, val)

		case cUnsignedInt64:
			val := it.customFields[i].prevFloatBits
			return it.lastIterated.TrySetFieldByNumber(fieldNum, val)

		case cSignedInt32:
			val := int32(it.customFields[i].prevFloatBits)
			return it.lastIterated.TrySetFieldByNumber(fieldNum, val)

		case cUnsignedInt32:
			val := uint32(it.customFields[i].prevFloatBits)
			return it.lastIterated.TrySetFieldByNumber(fieldNum, val)

		default:
			return fmt.Errorf(
				"%s expected custom int encoded field but field type was: %v",
				itErrPrefix, fieldType)
		}
	default:
		return fmt.Errorf(
			"%s unhandled fieldType: %v", itErrPrefix, fieldType)
	}
}

func (it *iterator) readFloatXOR(i int) (floatBits, xor uint64, err error) {
	xor, err = it.readXOR(i)
	if err != nil {
		return 0, 0, err
	}
	prevFloatBits := it.customFields[i].prevFloatBits
	return prevFloatBits ^ xor, xor, nil
}

func (it *iterator) readXOR(i int) (uint64, error) {
	cb, err := it.readBits(1)
	if err != nil {
		return 0, err
	}
	if cb == m3tsz.OpcodeZeroValueXOR {
		return 0, nil
	}

	cb2, err := it.readBits(1)
	if err != nil {
		return 0, err
	}

	cb = (cb << 1) | cb2
	if cb == m3tsz.OpcodeContainedValueXOR {
		var (
			previousXOR                       = it.customFields[i].prevXOR
			previousLeading, previousTrailing = encoding.LeadingAndTrailingZeros(previousXOR)
			numMeaningfulBits                 = 64 - previousLeading - previousTrailing
		)
		meaningfulBits, err := it.readBits(numMeaningfulBits)
		if err != nil {
			return 0, err
		}

		return meaningfulBits << uint(previousTrailing), nil
	}

	numLeadingZerosBits, err := it.readBits(6)
	if err != nil {
		return 0, err
	}
	numMeaningfulBitsBits, err := it.readBits(6)
	if err != nil {
		return 0, err
	}

	var (
		numLeadingZeros   = int(numLeadingZerosBits)
		numMeaningfulBits = int(numMeaningfulBitsBits) + 1
		numTrailingZeros  = 64 - numLeadingZeros - numMeaningfulBits
	)
	meaningfulBits, err := it.readBits(numMeaningfulBits)
	if err != nil {
		return 0, err
	}

	return meaningfulBits << uint(numTrailingZeros), nil
}

func (it *iterator) readFullFloatVal() (floatBits uint64, xor uint64, err error) {
	floatBits, err = it.readBits(64)
	if err != nil {
		return 0, 0, err
	}

	return floatBits, floatBits, nil
}

// readBitset does the inverse of encodeBitset on the encoder struct.
func (it *iterator) readBitset() error {
	it.bitsetValues = it.bitsetValues[:0]
	bitsetLengthBits, err := it.readVarInt()
	if err != nil {
		return err
	}

	for i := uint64(0); i < bitsetLengthBits; i++ {
		bit, err := it.stream.ReadBit()
		if err != nil {
			return fmt.Errorf("%s error reading bitset: %v", itErrPrefix, err)
		}

		if bit == opCodeBitsetValueIsSet {
			// Add 1 because protobuf fields are 1-indexed not 0-indexed.
			it.bitsetValues = append(it.bitsetValues, int(i)+1)
		}
	}

	return nil
}

func (it *iterator) readVarInt() (uint64, error) {
	var (
		// Convert array to slice and reset size to zero so
		// we can reuse the buffer.
		buf      = it.varIntBuf[:0]
		numBytes = 0
	)
	for {
		b, err := it.stream.ReadByte()
		if err != nil {
			return 0, fmt.Errorf("%s error reading var int: %v", itErrPrefix, err)
		}

		buf = append(buf, b)
		numBytes++

		if b>>7 == 0 {
			break
		}
	}

	buf = buf[:numBytes]
	varInt, _ := binary.Uvarint(buf)
	return varInt, nil
}

func (it *iterator) readIntSig(i int) error {
	updateControlBit, err := it.stream.ReadBit()
	if err != nil {
		return fmt.Errorf(
			"%s error reading int significant digits update control bit: %v",
			itErrPrefix, err)
	}
	if updateControlBit == opCodeNoChange {
		// No change.
		return nil
	}

	sigDigitsControlBit, err := it.stream.ReadBit()
	if err != nil {
		return fmt.Errorf(
			"%s error reading zero significant digits control bit: %v",
			itErrPrefix, err)
	}
	if sigDigitsControlBit == m3tsz.OpcodeZeroSig {
		it.customFields[i].intSigBitsTracker.NumSig = 0
	} else {
		numSigBits, err := it.readBits(6)
		if err != nil {
			return fmt.Errorf(
				"%s error reading number of significant digits: %v",
				itErrPrefix, err)
		}

		it.customFields[i].intSigBitsTracker.NumSig = uint8(numSigBits) + 1
	}

	return nil
}

func (it *iterator) readIntValDiff(i int) error {
	negativeControlBit, err := it.stream.ReadBit()
	if err != nil {
		return fmt.Errorf(
			"%s error reading negative control bit: %v",
			itErrPrefix, err)
	}

	numSig := int(it.customFields[i].intSigBitsTracker.NumSig)
	diffSigBits, err := it.readBits(numSig)
	if err != nil {
		return fmt.Errorf(
			"%s error reading significant digits: %v",
			itErrPrefix, err)
	}

	if it.customFields[i].fieldType == cUnsignedInt64 {
		diff := uint64(diffSigBits)
		shouldSubtract := false
		if negativeControlBit == opCodeIntDeltaNegative {
			shouldSubtract = true
		}

		prev := uint64(it.customFields[i].prevFloatBits)
		if shouldSubtract {
			it.customFields[i].prevFloatBits = prev - diff
		} else {
			it.customFields[i].prevFloatBits = prev + diff
		}
	} else {
		diff := int64(diffSigBits)
		sign := int64(1)
		if negativeControlBit == opCodeIntDeltaNegative {
			sign = -1.0
		}

		prev := int64(it.customFields[i].prevFloatBits)
		it.customFields[i].prevFloatBits = uint64(prev + (sign * diff))
	}

	return nil
}

// skipToNextByte will skip over any remaining bits in the current byte
// to reach the next byte. This is used in situations where the stream
// has padding bits to keep portions of data aligned at the byte boundary.
func (it *iterator) skipToNextByte() error {
	remainingBitsInByte := it.stream.RemainingBitsInCurrentByte()
	for remainingBitsInByte > 0 {
		_, err := it.stream.ReadBit()
		if err != nil {
			return err
		}
		remainingBitsInByte--
	}

	return nil
}

// TODO: Share logic with encoder if possible
func (it *iterator) moveToEndOfBytesDict(fieldIdx, i int) {
	existing := it.customFields[fieldIdx].iteratorBytesFieldDict
	for j := i; j < len(existing); j++ {
		nextIdx := j + 1
		if nextIdx >= len(existing) {
			break
		}

		currVal := existing[j]
		nextVal := existing[nextIdx]
		existing[j] = nextVal
		existing[nextIdx] = currVal
	}
}

// TODO: Share logic with encoder if possible
func (it *iterator) addToBytesDict(fieldIdx int, b []byte) {
	existing := it.customFields[fieldIdx].iteratorBytesFieldDict
	if len(existing) < it.byteFieldDictLRUSize {
		it.customFields[fieldIdx].iteratorBytesFieldDict = append(existing, b)
		return
	}

	// Shift everything down 1 and replace the last value to evict the
	// least recently used entry and add the newest one.
	//     [1,2,3]
	// becomes
	//     [2,3,3]
	// after shift, and then becomes
	//     [2,3,4]
	// after replacing the last value.
	for i := range existing {
		nextIdx := i + 1
		if nextIdx >= len(existing) {
			break
		}

		existing[i] = existing[nextIdx]
	}

	existing[len(existing)-1] = b
}

func (it *iterator) readBits(numBits int) (uint64, error) {
	res, err := it.stream.ReadBits(numBits)
	if err != nil {
		return 0, err
	}

	return res, nil
}

func (it *iterator) resetUnmarshalProtoBuffer(n int) {
	if it.unmarshalProtoBuf != nil && it.unmarshalProtoBuf.Cap() >= n {
		// If the existing one is big enough, just resize it.
		it.unmarshalProtoBuf.Resize(n)
		return
	}

	if it.unmarshalProtoBuf != nil {
		// If one exists, but its too small, return it to the pool.
		it.unmarshalProtoBuf.DecRef()
		it.unmarshalProtoBuf.Finalize()
	}

	// If none exists (or one existed but it was too small) get a new one
	// and IncRef(). DecRef() will never be called unless this one is
	// replaced by a new one later.
	it.unmarshalProtoBuf = it.opts.BytesPool().Get(n)
	it.unmarshalProtoBuf.IncRef()
	it.unmarshalProtoBuf.Resize(n)
}

func (it *iterator) hasNext() bool {
	return !it.hasError() && !it.isDone() && !it.isClosed()
}

func (it *iterator) hasError() bool {
	return it.err != nil
}

func (it *iterator) isDone() bool {
	return it.done
}

func (it *iterator) isClosed() bool {
	return it.closed
}
