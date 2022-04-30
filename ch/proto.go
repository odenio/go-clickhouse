package ch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/uptrace/go-clickhouse/ch/chpool"
	"github.com/uptrace/go-clickhouse/ch/chproto"
	"github.com/uptrace/go-clickhouse/ch/chschema"
)

const (
	clientName     = "go-clickhouse"
	chVersionMajor = 19
	chVersionMinor = 17
	chVersionPatch = 5
	chRevision     = 54428
)

type blockIter struct {
	db *DB
	cn *chpool.Conn

	stickyErr error
}

func newBlockIter(db *DB, cn *chpool.Conn) *blockIter {
	return &blockIter{
		db: db,
		cn: cn,
	}
}

func (it *blockIter) Close() error {
	if it.cn != nil {
		it.close()
	}
	return nil
}

func (it *blockIter) close() {
	it.db.releaseConn(it.cn, it.stickyErr)
	it.cn = nil
}

func (it *blockIter) Err() error {
	return it.stickyErr
}

func (it *blockIter) Next(ctx context.Context, block *chschema.Block) bool {
	if it.cn == nil {
		return false
	}

	ok, err := it.read(ctx, block)
	if err != nil {
		it.stickyErr = err
		it.close()
		return false
	}

	if !ok {
		it.close()
		return false
	}
	return true
}

func (it *blockIter) read(ctx context.Context, block *chschema.Block) (bool, error) {
	rd := it.cn.Reader(ctx, it.db.cfg.ReadTimeout)
	for {
		packet, err := rd.Uvarint()
		if err != nil {
			return false, err
		}

		switch packet {
		case chproto.ServerData:
			if err := readBlock(rd, block); err != nil {
				return false, err
			}
			return true, nil
		case chproto.ServerException:
			return false, readException(rd)
		case chproto.ServerProgress:
			if err := readProgress(rd); err != nil {
				return false, err
			}
		case chproto.ServerProfileInfo:
			if err := readProfileInfo(rd); err != nil {
				return false, err
			}
		case chproto.ServerTableColumns:
			if err := readServerTableColumns(rd); err != nil {
				return false, err
			}
		case chproto.ServerEndOfStream:
			return false, nil
		default:
			return false, fmt.Errorf("ch: blockIter.Next: unexpected packet: %d", packet)
		}
	}
}

func (db *DB) hello(ctx context.Context, cn *chpool.Conn) error {
	err := cn.WithWriter(ctx, db.cfg.WriteTimeout, func(wr *chproto.Writer) {
		wr.Uvarint(chproto.ClientHello)
		writeClientInfo(wr)

		wr.String(db.cfg.Database)
		wr.String(db.cfg.User)
		wr.String(db.cfg.Password)
	})
	if err != nil {
		return err
	}

	return cn.WithReader(ctx, db.cfg.ReadTimeout, func(rd *chproto.Reader) error {
		packet, err := rd.Uvarint()
		if err != nil {
			return err
		}
		switch packet {
		case chproto.ServerHello:
			return cn.ServerInfo.ReadFrom(rd)
		case chproto.ServerException:
			return readException(rd)
		default:
			return fmt.Errorf("ch: hello: unexpected packet: %d", packet)
		}
	})
}

func writeClientInfo(wr *chproto.Writer) {
	wr.String(clientName)
	wr.Uvarint(chVersionMajor)
	wr.Uvarint(chVersionMinor)
	wr.Uvarint(chRevision)
}

func readException(rd *chproto.Reader) (err error) {
	var exc Error

	if exc.Code, err = rd.Int32(); err != nil {
		return err
	}

	if exc.Name, err = rd.String(); err != nil {
		return err
	}

	if exc.Message, err = rd.String(); err != nil {
		return err
	}
	exc.Message = strings.TrimSpace(strings.TrimPrefix(exc.Message, exc.Name+":"))

	if exc.StackTrace, err = rd.String(); err != nil {
		return err
	}

	hasNested, err := rd.Bool()
	if err != nil {
		return err
	}
	if hasNested {
		exc.nested = readException(rd)
	}

	return &exc
}

func readProfileInfo(rd *chproto.Reader) error {
	if _, err := rd.Uvarint(); err != nil {
		return err
	}
	if _, err := rd.Uvarint(); err != nil {
		return err
	}
	if _, err := rd.Uvarint(); err != nil {
		return err
	}
	if _, err := rd.Bool(); err != nil {
		return err
	}
	if _, err := rd.Uvarint(); err != nil {
		return err
	}
	if _, err := rd.Bool(); err != nil {
		return err
	}
	return nil
}

func readProgress(rd *chproto.Reader) error {
	if _, err := rd.Uvarint(); err != nil {
		return err
	}
	if _, err := rd.Uvarint(); err != nil {
		return err
	}
	if _, err := rd.Uvarint(); err != nil {
		return err
	}
	if _, err := rd.Uvarint(); err != nil {
		return err
	}
	if _, err := rd.Uvarint(); err != nil {
		return err
	}
	return nil
}

func writePing(wr *chproto.Writer) {
	wr.Uvarint(chproto.ClientPing)
}

func readPong(rd *chproto.Reader) error {
	for {
		packet, err := rd.Uvarint()
		if err != nil {
			return err
		}

		switch packet {
		case chproto.ServerPong:
			return nil
		case chproto.ServerException:
			return readException(rd)
		case chproto.ServerEndOfStream:
			return nil
		default:
			return fmt.Errorf("ch: readPong: unexpected packet: %d", packet)
		}
	}
}

var hostname string

func (db *DB) writeQuery(wr *chproto.Writer, query string) {
	if hostname == "" {
		hostname, _ = os.Hostname()
	}

	wr.Uvarint(chproto.ClientQuery)
	wr.String("")

	// TODO: use QuerySecondary - https://github.com/ClickHouse/ClickHouse/blob/master/dbms/src/Client/Connection.cpp#L388-L404
	wr.Uvarint(chproto.QueryInitial)
	wr.String("") // initial user
	wr.String("") // initial query id
	wr.String("[::ffff:127.0.0.1]:0")
	wr.Uvarint(1) // iface type TCP
	wr.String(hostname)
	wr.String(hostname)
	writeClientInfo(wr)
	wr.String("")              // quota key
	wr.Uvarint(chVersionPatch) // client version patch

	db.writeSettings(wr)

	wr.Uvarint(2)
	wr.Uvarint(chproto.CompressionEnabled)
	wr.String(query)
}

func (db *DB) writeSettings(wr *chproto.Writer) {
	for key, value := range db.cfg.QuerySettings {
		wr.String(key)
		switch value := value.(type) {
		case string:
			wr.String(value)
		case int:
			wr.Uvarint(uint64(value))
		case int64:
			wr.Uvarint(uint64(value))
		case uint64:
			wr.Uvarint(value)
		case bool:
			wr.Bool(value)
		default:
			panic(fmt.Errorf("%s setting has unsupported type: %T", key, value))
		}
	}

	wr.String("")
}

var emptyBlock chschema.Block

func writeBlock(ctx context.Context, wr *chproto.Writer, block *chschema.Block) {
	if block == nil {
		block = &emptyBlock
	}
	wr.Uvarint(chproto.ClientData)
	wr.String("")

	wr.WithCompression(func() error {
		writeBlockInfo(wr)
		return block.WriteTo(wr)
	})
}

func writeBlockInfo(wr *chproto.Writer) {
	wr.Uvarint(1)
	wr.Bool(false)

	wr.Uvarint(2)
	wr.Int32(-1)

	wr.Uvarint(0)
}

func readSampleBlock(rd *chproto.Reader) (*chschema.Block, error) {
	for {
		packet, err := rd.Uvarint()
		if err != nil {
			return nil, err
		}

		switch packet {
		case chproto.ServerData:
			block := new(chschema.Block)
			if err := readBlock(rd, block); err != nil {
				return nil, err
			}
			return block, nil
		case chproto.ServerTableColumns:
			if err := readServerTableColumns(rd); err != nil {
				return nil, err
			}
		case chproto.ServerException:
			return nil, readException(rd)
		default:
			return nil, fmt.Errorf("ch: readSampleBlock: unexpected packet: %d", packet)
		}
	}
}

func readDataBlocks(rd *chproto.Reader) (*result, error) {
	var res *result
	block := new(chschema.Block)
	for {
		packet, err := rd.Uvarint()
		if err != nil {
			return nil, err
		}

		switch packet {
		case chproto.ServerData:
			if err := readBlock(rd, block); err != nil {
				return nil, err
			}

			if res == nil {
				res = new(result)
			}
			res.affected += block.NumRow
		case chproto.ServerException:
			return nil, readException(rd)
		case chproto.ServerProgress:
			if err := readProgress(rd); err != nil {
				return nil, err
			}
		case chproto.ServerProfileInfo:
			if err := readProfileInfo(rd); err != nil {
				return nil, err
			}
		case chproto.ServerTableColumns:
			if err := readServerTableColumns(rd); err != nil {
				return nil, err
			}
		case chproto.ServerEndOfStream:
			return res, nil
		default:
			return nil, fmt.Errorf("ch: readDataBlocks: unexpected packet: %d", packet)
		}
	}
}

func readPacket(rd *chproto.Reader) (*result, error) {
	packet, err := rd.Uvarint()
	if err != nil {
		return nil, err
	}

	res := new(result)
	switch packet {
	case chproto.ServerException:
		return nil, readException(rd)
	case chproto.ServerProgress:
		if err := readProgress(rd); err != nil {
			return nil, err
		}
		return res, nil
	case chproto.ServerProfileInfo:
		if err := readProfileInfo(rd); err != nil {
			return nil, err
		}
		return res, nil
	case chproto.ServerTableColumns:
		if err := readServerTableColumns(rd); err != nil {
			return nil, err
		}
		return res, nil
	case chproto.ServerEndOfStream:
		return res, nil
	default:
		return nil, fmt.Errorf("ch: readPacket: unexpected packet: %d", packet)
	}
}

func readBlock(rd *chproto.Reader, block *chschema.Block) error {
	if _, err := rd.String(); err != nil {
		return err
	}

	return rd.WithCompression(func() error {
		if err := readBlockInfo(rd); err != nil {
			return err
		}

		numColumn, err := rd.Uvarint()
		if err != nil {
			return err
		}
		numRow, err := rd.Uvarint()
		if err != nil {
			return err
		}

		block.NumColumn = int(numColumn)
		block.NumRow = int(numRow)

		for i := 0; i < int(numColumn); i++ {
			colName, err := rd.String()
			if err != nil {
				return err
			}
			if colName == "" {
				return errors.New("ch: column has empty name")
			}

			colType, err := rd.String()
			if err != nil {
				return err
			}
			if colType == "" {
				return fmt.Errorf("ch: column=%s has empty type", colName)
			}

			col := block.Column(colName, colType)
			if err := col.ReadFrom(rd, int(numRow)); err != nil {
				return err
			}
		}

		return nil
	})
}

func readBlockInfo(rd *chproto.Reader) error {
	if _, err := rd.Uvarint(); err != nil {
		return err
	}
	if _, err := rd.Bool(); err != nil {
		return err
	}

	if _, err := rd.Uvarint(); err != nil {
		return err
	}
	if _, err := rd.Int32(); err != nil {
		return err
	}

	if _, err := rd.Uvarint(); err != nil {
		return err
	}

	return nil
}

func writeCancel(wr *chproto.Writer) {
	wr.Uvarint(chproto.ClientCancel)
}

func readServerTableColumns(rd *chproto.Reader) error {
	_, err := rd.String()
	if err != nil {
		return err
	}
	_, err = rd.String()
	if err != nil {
		return err
	}
	return nil
}
