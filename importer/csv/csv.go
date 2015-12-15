package csv

import (
	"github.com/dfjones/riprdio/importer"
	log "github.com/Sirupsen/logrus"
	"io"
	"encoding/csv"
	"errors"
)

func init() {
	importer.RegisterFormat("RdioEnhancerCsv", "text/csv", Parse)
}

func Parse(reader io.Reader) ([]*importer.SpotifySong, error) {
	records, err := csv.NewReader(reader).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, errors.New("CSV appears to have no records")
	}
	songs := make([]*importer.SpotifySong, 0)
	for _, r := range records[1:] {
		if len(r) < 2 {
			log.Warn("Malformed record", r)
		} else {
			songs = append(songs, &importer.SpotifySong{Name: r[0], Artist: r[1], Album: r[2]})
		}
	}
	log.Infof("Parsed Rdio Enhancer Csv len %d", len(records))
	return songs, nil
}
