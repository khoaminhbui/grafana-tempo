package wal

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/golang/protobuf/proto" //nolint:all
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/pkg/tempopb"
	"github.com/grafana/tempo/pkg/util/test"
	"github.com/grafana/tempo/tempodb/backend"
	v2 "github.com/grafana/tempo/tempodb/encoding/v2"
)

const (
	testTenantID = "fake"
)

type mockCombiner struct {
}

func (m *mockCombiner) Combine(dataEncoding string, objs ...[]byte) ([]byte, bool, error) {
	if len(objs) != 2 {
		return nil, false, nil
	}

	if len(objs[0]) > len(objs[1]) {
		return objs[0], true, nil
	}

	return objs[1], true, nil
}

func TestAppend(t *testing.T) {
	wal, err := New(&Config{
		Filepath: t.TempDir(),
	})
	require.NoError(t, err, "unexpected error creating temp wal")

	blockID := uuid.New()

	block, err := wal.NewBlock(blockID, testTenantID, "")
	require.NoError(t, err, "unexpected error creating block")

	numMsgs := 100
	reqs := make([]*tempopb.Trace, 0, numMsgs)
	for i := 0; i < numMsgs; i++ {
		req := test.MakeTrace(rand.Int()%1000, []byte{0x01})
		reqs = append(reqs, req)
		bReq, err := proto.Marshal(req)
		require.NoError(t, err)
		err = block.Append([]byte{0x01}, bReq, 0, 0)
		require.NoError(t, err, "unexpected error writing req")
	}

	records := block.appender.Records()
	file, err := block.file()
	require.NoError(t, err)

	dataReader, err := v2.NewDataReader(backend.NewContextReaderWithAllReader(file), backend.EncNone)
	require.NoError(t, err)
	iterator := v2.NewRecordIterator(records, dataReader, v2.NewObjectReaderWriter())
	defer iterator.Close()
	i := 0

	for {
		_, bytesObject, err := iterator.Next(context.Background())
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		req := &tempopb.Trace{}
		err = proto.Unmarshal(bytesObject, req)
		require.NoError(t, err)

		require.True(t, proto.Equal(req, reqs[i]))
		i++
	}
	require.Equal(t, numMsgs, i)
}

func TestCompletedDirIsRemoved(t *testing.T) {
	// Create /completed/testfile and verify it is removed.

	tempDir := t.TempDir()

	err := os.MkdirAll(path.Join(tempDir, completedDir), os.ModePerm)
	require.NoError(t, err, "unexpected error creating completedDir")

	_, err = os.Create(path.Join(tempDir, completedDir, "testfile"))
	require.NoError(t, err, "unexpected error creating testfile")

	_, err = New(&Config{
		Filepath: tempDir,
	})
	require.NoError(t, err, "unexpected error creating temp wal")

	_, err = os.Stat(path.Join(tempDir, completedDir))
	require.Error(t, err, "completedDir should not exist")
}

func TestErrorConditions(t *testing.T) {
	tempDir := t.TempDir()

	wal, err := New(&Config{
		Filepath: tempDir,
		Encoding: backend.EncGZIP,
	})
	require.NoError(t, err, "unexpected error creating temp wal")

	blockID := uuid.New()

	// create partially corrupt block
	block, err := wal.NewBlock(blockID, testTenantID, "")
	require.NoError(t, err, "unexpected error creating block")

	objects := 10
	for i := 0; i < objects; i++ {
		id := make([]byte, 16)
		rand.Read(id)
		obj := test.MakeTrace(rand.Int()%10, id)
		bObj, err := proto.Marshal(obj)
		require.NoError(t, err)

		err = block.Append(id, bObj, 0, 0)
		require.NoError(t, err, "unexpected error writing req")
	}
	appendFile, err := os.OpenFile(block.fullFilename(), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	require.NoError(t, err)
	_, err = appendFile.Write([]byte{0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01})
	require.NoError(t, err)
	err = appendFile.Close()
	require.NoError(t, err)

	// create unparseable filename
	err = os.WriteFile(filepath.Join(tempDir, "fe0b83eb-a86b-4b6c-9a74-dc272cd5700e:tenant:v2:notanencoding"), []byte{}, 0644)
	require.NoError(t, err)

	// create empty block
	err = os.WriteFile(filepath.Join(tempDir, "fe0b83eb-a86b-4b6c-9a74-dc272cd5700e:blerg:v2:gzip"), []byte{}, 0644)
	require.NoError(t, err)

	blocks, err := wal.RescanBlocks(func([]byte, string) (uint32, uint32, error) {
		return 0, 0, nil
	}, 0, log.NewNopLogger())
	require.NoError(t, err, "unexpected error getting blocks")
	require.Len(t, blocks, 1)

	require.Equal(t, objects, blocks[0].appender.Length())

	// confirm block has been removed
	require.NoFileExists(t, filepath.Join(tempDir, "fe0b83eb-a86b-4b6c-9a74-dc272cd5700e:tenant:v2:gzip"))
	require.NoFileExists(t, filepath.Join(tempDir, "fe0b83eb-a86b-4b6c-9a74-dc272cd5700e:blerg:v2:gzip"))
}

func TestAppendBlockStartEnd(t *testing.T) {
	wal, err := New(&Config{
		Filepath:       t.TempDir(),
		Encoding:       backend.EncNone,
		IngestionSlack: 2 * time.Minute,
	})
	require.NoError(t, err, "unexpected error creating temp wal")

	blockID := uuid.New()
	block, err := wal.NewBlock(blockID, testTenantID, "")
	require.NoError(t, err, "unexpected error creating block")

	// create a new block and confirm start/end times are correct
	blockStart := uint32(time.Now().Unix())
	blockEnd := uint32(time.Now().Add(time.Minute).Unix())

	for i := 0; i < 10; i++ {
		bytes := make([]byte, 16)
		rand.Read(bytes)

		err = block.Append(bytes, bytes, blockStart, blockEnd)
		require.NoError(t, err, "unexpected error writing req")
	}

	require.Equal(t, blockStart, uint32(block.meta.StartTime.Unix()))
	require.Equal(t, blockEnd, uint32(block.meta.EndTime.Unix()))

	// rescan the block and make sure that start/end times are correct
	blockStart = uint32(time.Now().Add(-time.Hour).Unix())
	blockEnd = uint32(time.Now().Unix())

	blocks, err := wal.RescanBlocks(func([]byte, string) (uint32, uint32, error) {
		return blockStart, blockEnd, nil
	}, time.Hour, log.NewNopLogger())
	require.NoError(t, err, "unexpected error getting blocks")
	require.Len(t, blocks, 1)

	require.Equal(t, blockStart, uint32(blocks[0].meta.StartTime.Unix()))
	require.Equal(t, blockEnd, uint32(blocks[0].meta.EndTime.Unix()))
}

func TestAdjustTimeRangeForSlack(t *testing.T) {
	a := &AppendBlock{
		meta: &backend.BlockMeta{
			TenantID: "test",
		},
		ingestionSlack: 2 * time.Minute,
	}

	// test happy path
	start := uint32(time.Now().Unix())
	end := uint32(time.Now().Unix())
	actualStart, actualEnd := a.adjustTimeRangeForSlack(start, end, 0)
	assert.Equal(t, start, actualStart)
	assert.Equal(t, end, actualEnd)

	// test start out of range
	now := uint32(time.Now().Unix())
	start = uint32(time.Now().Add(-time.Hour).Unix())
	end = uint32(time.Now().Unix())
	actualStart, actualEnd = a.adjustTimeRangeForSlack(start, end, 0)
	assert.Equal(t, now, actualStart)
	assert.Equal(t, end, actualEnd)

	// test end out of range
	now = uint32(time.Now().Unix())
	start = uint32(time.Now().Unix())
	end = uint32(time.Now().Add(time.Hour).Unix())
	actualStart, actualEnd = a.adjustTimeRangeForSlack(start, end, 0)
	assert.Equal(t, start, actualStart)
	assert.Equal(t, now, actualEnd)

	// test additional start slack honored
	start = uint32(time.Now().Add(-time.Hour).Unix())
	end = uint32(time.Now().Unix())
	actualStart, actualEnd = a.adjustTimeRangeForSlack(start, end, time.Hour)
	assert.Equal(t, start, actualStart)
	assert.Equal(t, end, actualEnd)
}

func TestAppendReplayFind(t *testing.T) {
	for _, e := range backend.SupportedEncoding {
		t.Run(e.String(), func(t *testing.T) {
			testAppendReplayFind(t, backend.EncZstd)
		})
	}
}

func testAppendReplayFind(t *testing.T, e backend.Encoding) {
	wal, err := New(&Config{
		Filepath: t.TempDir(),
		Encoding: e,
	})
	require.NoError(t, err, "unexpected error creating temp wal")

	blockID := uuid.New()

	block, err := wal.NewBlock(blockID, testTenantID, "")
	require.NoError(t, err, "unexpected error creating block")

	objects := 1000
	objs := make([][]byte, 0, objects)
	ids := make([][]byte, 0, objects)
	for i := 0; i < objects; i++ {
		id := make([]byte, 16)
		rand.Read(id)
		obj := test.MakeTrace(rand.Int()%10, id)
		ids = append(ids, id)
		bObj, err := proto.Marshal(obj)
		require.NoError(t, err)
		objs = append(objs, bObj)

		err = block.Append(id, bObj, 0, 0)
		require.NoError(t, err, "unexpected error writing req")
	}

	for i, id := range ids {
		obj, err := block.Find(id, &mockCombiner{})
		require.NoError(t, err)
		require.Equal(t, objs[i], obj)
	}

	// write garbage data at the end to confirm a partial block will load
	appendFile, err := os.OpenFile(block.fullFilename(), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	require.NoError(t, err)
	_, err = appendFile.Write([]byte{0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01})
	require.NoError(t, err)
	err = appendFile.Close()
	require.NoError(t, err)

	blocks, err := wal.RescanBlocks(func([]byte, string) (uint32, uint32, error) {
		return 0, 0, nil
	}, 0, log.NewNopLogger())
	require.NoError(t, err, "unexpected error getting blocks")
	require.Len(t, blocks, 1)

	iterator, err := blocks[0].Iterator(&mockCombiner{})
	require.NoError(t, err)
	defer iterator.Close()

	// append block find
	for i, id := range ids {
		obj, err := blocks[0].Find(id, &mockCombiner{})
		require.NoError(t, err)
		require.Equal(t, objs[i], obj)
	}

	i := 0
	for {
		id, obj, err := iterator.Next(context.Background())
		if err == io.EOF {
			break
		} else {
			require.NoError(t, err)
		}

		found := false
		j := 0
		for ; j < len(ids); j++ {
			if bytes.Equal(ids[j], id) {
				found = true
				break
			}
		}

		require.True(t, found)
		require.Equal(t, objs[j], obj)
		require.Equal(t, ids[j], []byte(id))
		i++
	}

	require.Equal(t, objects, i)

	err = blocks[0].Clear()
	require.NoError(t, err)
}

func TestParseFilename(t *testing.T) {
	tests := []struct {
		name                 string
		filename             string
		expectUUID           uuid.UUID
		expectTenant         string
		expectedVersion      string
		expectedEncoding     backend.Encoding
		expectedDataEncoding string
		expectError          bool
	}{
		{
			name:                 "version, enc snappy and dataencoding",
			filename:             "123e4567-e89b-12d3-a456-426614174000:foo:v2:snappy:dataencoding",
			expectUUID:           uuid.MustParse("123e4567-e89b-12d3-a456-426614174000"),
			expectTenant:         "foo",
			expectedVersion:      "v2",
			expectedEncoding:     backend.EncSnappy,
			expectedDataEncoding: "dataencoding",
		},
		{
			name:                 "version, enc none and dataencoding",
			filename:             "123e4567-e89b-12d3-a456-426614174000:foo:v2:none:dataencoding",
			expectUUID:           uuid.MustParse("123e4567-e89b-12d3-a456-426614174000"),
			expectTenant:         "foo",
			expectedVersion:      "v2",
			expectedEncoding:     backend.EncNone,
			expectedDataEncoding: "dataencoding",
		},
		{
			name:                 "empty dataencoding",
			filename:             "123e4567-e89b-12d3-a456-426614174000:foo:v2:snappy",
			expectUUID:           uuid.MustParse("123e4567-e89b-12d3-a456-426614174000"),
			expectTenant:         "foo",
			expectedVersion:      "v2",
			expectedEncoding:     backend.EncSnappy,
			expectedDataEncoding: "",
		},
		{
			name:                 "empty dataencoding with semicolon",
			filename:             "123e4567-e89b-12d3-a456-426614174000:foo:v2:snappy:",
			expectUUID:           uuid.MustParse("123e4567-e89b-12d3-a456-426614174000"),
			expectTenant:         "foo",
			expectedVersion:      "v2",
			expectedEncoding:     backend.EncSnappy,
			expectedDataEncoding: "",
		},
		{
			name:        "path fails",
			filename:    "/blerg/123e4567-e89b-12d3-a456-426614174000:foo",
			expectError: true,
		},
		{
			name:        "no :",
			filename:    "123e4567-e89b-12d3-a456-426614174000",
			expectError: true,
		},
		{
			name:        "empty string",
			filename:    "",
			expectError: true,
		},
		{
			name:        "bad uuid",
			filename:    "123e4:foo",
			expectError: true,
		},
		{
			name:        "no tenant",
			filename:    "123e4567-e89b-12d3-a456-426614174000:",
			expectError: true,
		},
		{
			name:        "no version",
			filename:    "123e4567-e89b-12d3-a456-426614174000:test::none",
			expectError: true,
		},
		{
			name:        "wrong splits - 6",
			filename:    "123e4567-e89b-12d3-a456-426614174000:test:test:test:test:test",
			expectError: true,
		},
		{
			name:        "wrong splits - 3",
			filename:    "123e4567-e89b-12d3-a456-426614174000:test:test",
			expectError: true,
		},
		{
			name:        "wrong splits - 1",
			filename:    "123e4567-e89b-12d3-a456-426614174000",
			expectError: true,
		},
		{
			name:        "bad encoding",
			filename:    "123e4567-e89b-12d3-a456-426614174000:test:v1:asdf",
			expectError: true,
		},
		{
			name:        "ez-mode old format",
			filename:    "123e4567-e89b-12d3-a456-426614174000:foo",
			expectError: true,
		},
		{
			name:        "deprecated version",
			filename:    "123e4567-e89b-12d3-a456-426614174000:foo:v1:snappy",
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actualUUID, actualTenant, actualVersion, actualEncoding, actualDataEncoding, err := ParseFilename(tc.filename)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectUUID, actualUUID)
			require.Equal(t, tc.expectTenant, actualTenant)
			require.Equal(t, tc.expectedEncoding, actualEncoding)
			require.Equal(t, tc.expectedVersion, actualVersion)
			require.Equal(t, tc.expectedDataEncoding, actualDataEncoding)
		})
	}
}

func BenchmarkWALNone(b *testing.B) {
	benchmarkWriteFindReplay(b, backend.EncNone)
}
func BenchmarkWALSnappy(b *testing.B) {
	benchmarkWriteFindReplay(b, backend.EncSnappy)
}
func BenchmarkWALLZ4(b *testing.B) {
	benchmarkWriteFindReplay(b, backend.EncLZ4_1M)
}
func BenchmarkWALGZIP(b *testing.B) {
	benchmarkWriteFindReplay(b, backend.EncGZIP)
}
func BenchmarkWALZSTD(b *testing.B) {
	benchmarkWriteFindReplay(b, backend.EncZstd)
}

func BenchmarkWALS2(b *testing.B) {
	benchmarkWriteFindReplay(b, backend.EncS2)
}

func benchmarkWriteFindReplay(b *testing.B, encoding backend.Encoding) {
	objects := 1000
	objs := make([][]byte, 0, objects)
	ids := make([][]byte, 0, objects)
	for i := 0; i < objects; i++ {
		id := make([]byte, 16)
		rand.Read(id)
		obj := make([]byte, rand.Intn(100)+1)
		rand.Read(obj)
		ids = append(ids, id)
		objs = append(objs, obj)
	}
	mockCombiner := &mockCombiner{}
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		wal, _ := New(&Config{
			Filepath: b.TempDir(),
			Encoding: encoding,
		})

		blockID := uuid.New()
		block, err := wal.NewBlock(blockID, testTenantID, "")
		require.NoError(b, err)

		// write
		for j, obj := range objs {
			err := block.Append(ids[j], obj, 0, 0)
			require.NoError(b, err)
		}

		// find
		for _, id := range ids {
			_, err := block.Find(id, mockCombiner)
			require.NoError(b, err)
		}

		// replay
		_, err = wal.RescanBlocks(func([]byte, string) (uint32, uint32, error) {
			return 0, 0, nil
		}, 0, log.NewNopLogger())
		require.NoError(b, err)
	}
}
