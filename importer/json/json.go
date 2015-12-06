package json

import (
	"github.com/dfjones/riprdio/importer"
	"github.com/labstack/gommon/log"
	"io"
	"encoding/json"
)

func init() {
	importer.RegisterFormat("ConservatorioJson", "application/json", Parse)
}

func Parse(reader io.Reader) ([]*importer.SpotifySong, error) {
	decoder := json.NewDecoder(reader)
	var root map[string]interface{}
	err := decoder.Decode(&root)
	if err != nil {
		return nil, err
	}
	songs := make([]*importer.SpotifySong, 0)
	var objects map[string]interface{}
	objects = root["objects"].(map[string]interface{})
	for _,o := range objects {
		odata := (o).(map[string]interface{})
		artistField := odata["albumArtist"]
		if artistField == nil {
			// probably not a song, skip it
			continue
		}
		artist := artistField.(string)
		album := odata["album"].(string)
		name := odata["name"].(string)
		songs = append(songs, &importer.SpotifySong{Name: name, Artist: artist, Album: album})
	}
	log.Info("Parsed Conservatorio Json len %d", len(songs))
	return songs, nil
}
