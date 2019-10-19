package influxdb

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	// this is important because of the bug in go mod
	_ "github.com/influxdata/influxdb1-client"
	client "github.com/influxdata/influxdb1-client/v2"
	"github.com/juju/loggo"
	"github.com/pkg/errors"

	"github.com/gabriel-samfira/coriolis-logger/config"
	"github.com/gabriel-samfira/coriolis-logger/datastore/common"
	"github.com/gabriel-samfira/coriolis-logger/logging"
	"github.com/gabriel-samfira/coriolis-logger/params"
)

var log = loggo.GetLogger("coriolis.logger.datastore.influxdb")

func NewInfluxDBDatastore(ctx context.Context, cfg *config.InfluxDB) (common.DataStore, error) {
	if err := cfg.Validate(); err != nil {
		return nil, errors.Wrap(err, "validating influx config")
	}

	store := &InfluxDBDataStore{
		cfg:      cfg,
		points:   []*client.Point{},
		ctx:      ctx,
		closed:   make(chan struct{}),
		quit:     make(chan struct{}),
		flushNow: make(chan int, 10),
		flushed:  make(chan int, 10),
	}

	if err := store.connect(); err != nil {
		return nil, errors.Wrap(err, "connecting to influxdb")
	}
	return store, nil
}

var _ common.DataStore = (*InfluxDBDataStore)(nil)

type InfluxDBDataStore struct {
	cfg      *config.InfluxDB
	con      client.Client
	mut      sync.Mutex
	points   []*client.Point
	ctx      context.Context
	closed   chan struct{}
	quit     chan struct{}
	flushNow chan int
	flushed  chan int
}

func (i *InfluxDBDataStore) doWork() {
	var interval int
	if i.cfg.WriteInterval == 0 {
		interval = 1
	} else {
		interval = i.cfg.WriteInterval
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer func() {
		ticker.Stop()
		close(i.closed)
	}()
	for {
		select {
		case <-i.ctx.Done():
			return
		case <-ticker.C:
			if err := i.flush(); err != nil {
				log.Errorf("failed to flush logs to backend: %v", err)
			}
		case <-i.flushNow:
			i.mut.Unlock()
			if err := i.flush(); err != nil {
				log.Errorf("failed to flush logs to backend: %v", err)
			}
			i.mut.Lock()
			i.flushed <- 1
		case <-i.quit:
			return
		}
	}
}

func (i *InfluxDBDataStore) Start() error {
	go i.doWork()
	return nil
}

func (i *InfluxDBDataStore) Stop() error {
	close(i.quit)
	i.Wait()
	return nil
}

func (i *InfluxDBDataStore) Wait() {
	<-i.closed
}

func (i *InfluxDBDataStore) connect() error {
	i.mut.Lock()
	defer i.mut.Unlock()
	tlsCfg, err := i.cfg.TLSConfig()
	if err != nil {
		return errors.Wrap(err, "getting TLS config for influx client")
	}
	conf := client.HTTPConfig{
		Addr:      i.cfg.URL.String(),
		Username:  i.cfg.Username,
		Password:  i.cfg.Password,
		TLSConfig: tlsCfg,
	}
	con, err := client.NewHTTPClient(conf)
	if err != nil {
		return errors.Wrap(err, "getting influx connection")
	}
	i.con = con
	return nil
}

func (i *InfluxDBDataStore) flush() error {
	i.mut.Lock()
	defer i.mut.Unlock()
	bp, err := client.NewBatchPoints(client.BatchPointsConfig{
		Database:  i.cfg.Database,
		Precision: "ns",
	})
	if err != nil {
		return errors.Wrap(err, "getting influx batch point")
	}
	if i.points != nil && len(i.points) > 0 {
		for _, val := range i.points {
			bp.AddPoint(val)
		}
		if err := i.con.Write(bp); err != nil {
			return errors.Wrap(err, "writing log line to influx")
		}
		i.points = []*client.Point{}
	}
	return nil
}

func (i *InfluxDBDataStore) Write(logMsg logging.LogMessage) (err error) {
	i.mut.Lock()
	defer i.mut.Unlock()
	tags := map[string]string{
		"hostname": logMsg.Hostname,
		"severity": logMsg.Severity.String(),
		"facility": logMsg.Facility.String(),
	}
	fields := map[string]interface{}{
		"message": logMsg.Message,
	}

	var tm time.Time = logMsg.Timestamp
	if logMsg.RFC == logging.RFC3164 {
		tm = time.Now()
	}
	pt, err := client.NewPoint(logMsg.BinaryName, tags, fields, tm)
	if err != nil {
		return errors.Wrap(err, "adding new log message point")
	}
	i.points = append(i.points, pt)

	if len(i.points) >= 20000 {
		i.flushNow <- 1
		select {
		case <-i.flushed:
		case <-time.After(60 * time.Second):
			return fmt.Errorf("timed out flushing logs")
		}
	}
	return nil
}

func (i *InfluxDBDataStore) Rotate(olderThan time.Time) error {
	return nil
}

func (i *InfluxDBDataStore) ResultReader(p params.QueryParams) common.Reader {
	return &influxDBReader{
		datastore: i,
		params:    p,
	}
}

func (i *InfluxDBDataStore) List() ([]string, error) {
	query := client.NewQuery("SHOW MEASUREMENTS", i.cfg.Database, "ns")
	resp, err := i.con.QueryAsChunk(query)
	if err != nil {
		return nil, errors.Wrap(err, "listing logs")
	}
	ret := []string{}
	for {
		r, err := resp.NextResponse()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, errors.Wrap(err, "fetching response")
		}
		for _, result := range r.Results {
			for _, serie := range result.Series {
				for _, val := range serie.Values {
					if len(val) == 0 {
						continue
					}
					ret = append(ret, val[0].(string))
				}
			}
		}
	}
	return ret, nil
}

func (i *InfluxDBDataStore) Query(q client.Query) (*client.ChunkedResponse, error) {
	resp, err := i.con.QueryAsChunk(q)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

type influxDBReader struct {
	datastore *InfluxDBDataStore
	params    params.QueryParams

	result *client.ChunkedResponse
	done   bool
}

func (i *influxDBReader) prepareQuery() (string, error) {
	if i.params.BinaryName == "" {
		return "", fmt.Errorf("missing application name")
	}
	undefinedDate := time.Time{}
	q := fmt.Sprintf(`select time,severity,message from %s`, i.params.BinaryName)
	if !i.params.StartDate.Equal(undefinedDate) || !i.params.EndDate.Equal(undefinedDate) || i.params.Hostname != "" {
		q += ` where `
	}

	options := []string{}

	if !i.params.StartDate.Equal(undefinedDate) {
		options = append(
			options,
			fmt.Sprintf(`time >= %d`, i.params.StartDate.UnixNano()))
	} else if !i.params.EndDate.Equal(undefinedDate) {
		options = append(
			options,
			fmt.Sprintf(`time <= %d`, i.params.EndDate.UnixNano()))

	}
	if i.params.Hostname != "" {
		options = append(options, fmt.Sprintf(`hostname='%s'`, i.params.Hostname))
	}
	// if i.params.Severity != 0 {
	// 	options = append(options, fmt.Sprintf(`severity < '%d'`, i.params.Severity))
	// }
	if len(options) > 0 {
		q += strings.Join(options, ` and `)
	}
	return q, nil
}

var _ common.Reader = (*influxDBReader)(nil)

func (i *influxDBReader) ReadNext() ([]byte, error) {
	if i.result == nil {
		i.datastore.flush()
		query, err := i.prepareQuery()
		if err != nil {
			return nil, errors.Wrap(err, "preparing query")
		}
		influxQ := client.NewQuery(query, i.datastore.cfg.Database, "ns")
		influxQ.ChunkSize = 20000
		resp, err := i.datastore.con.QueryAsChunk(influxQ)
		if err != nil {
			return nil, errors.Wrap(err, "executing query")
		}
		i.result = resp
	}

	res, err := i.result.NextResponse()
	if err != nil {
		if err == io.EOF {
			return nil, err
		}
		return nil, errors.Wrap(err, "reading results")
	}
	newline := []byte("\n")
	buf := bytes.NewBuffer([]byte{})
	for _, result := range res.Results {
		for _, serie := range result.Series {
			for _, val := range serie.Values {
				line := []byte(val[2].(string))
				if len(line) > 0 && line[len(line)-1] != newline[0] {
					line = append(line, []byte("\n")...)
				}
				_, err := buf.Write(line)
				if err != nil {
					return nil, errors.Wrap(err, "reading value")
				}
			}
		}
	}
	contents := buf.Bytes()
	buf.Reset()
	return contents, nil
}
