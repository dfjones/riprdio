package importer

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"github.com/dfjones/riprdio/config"
	"github.com/dfjones/riprdio/token"
	"github.com/labstack/echo"
	"github.com/labstack/gommon/log"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const (
	searchUrl   = "https://api.spotify.com/v1/search"
	concurrency = 10
	maxRetries  = 10
)

var (
	pipelines = pipelineStates{sync.Mutex{}, make(map[string]*PipelineState)}
)

type SpotifySong struct {
	Name            string
	Artist          string
	Album           string
	SpotifyAlbumUri string
	SpotifyTrackUri string
}

type pipelineStates struct {
	mutex   sync.Mutex
	running map[string]*PipelineState
}

type PipelineState struct {
	Id                  string
	ProcessedSongs      chan *SpotifySong
	Stats               PipelineStats
	mx                  sync.Mutex
	progressSubscribers []chan *ProgressMessage
}

type PipelineStats struct {
	ImportSize      int
	FoundAlbums     int
	FoundTracks     int
	TotalFound      int
	NotFound        int
	ProgressPercent float64
}

type ProgressMessage struct {
	Stats        PipelineStats
	NotFoundSong *SpotifySong
}

type searchJson map[string]interface{}

func (p *PipelineState) CreateSubscriber() chan *ProgressMessage {
	c := make(chan *ProgressMessage)
	p.mx.Lock()
	defer p.mx.Unlock()
	p.progressSubscribers = append(p.progressSubscribers, c)
	return c
}

func (p *PipelineState) RemoveSubscriber(s chan *ProgressMessage) {
	p.mx.Lock()
	defer p.mx.Unlock()
	for i, c := range p.progressSubscribers {
		if c == s {
			p.progressSubscribers = append(p.progressSubscribers[:i], p.progressSubscribers[i+1:]...)
			return
		}
	}
}

func (p *PipelineState) GetSubscribers() []chan *ProgressMessage {
	p.mx.Lock()
	defer p.mx.Unlock()
	s := make([]chan *ProgressMessage, len(p.progressSubscribers))
	copy(s, p.progressSubscribers)
	return s
}

func RunImportPipeline(context *echo.Context, reader io.Reader) (*PipelineState, error) {
	songs, err := Parse(reader)
	if err != nil {
		return nil, err
	}
	state, err := Process(context, songs)
	if err != nil {
		return state, err
	}
	addRunningPipeline(state)
	return state, nil
}

func GetRunningPipeline(id string) *PipelineState {
	pipelines.mutex.Lock()
	defer pipelines.mutex.Unlock()
	return pipelines.running[id]
}

func addRunningPipeline(pipeline *PipelineState) {
	pipelines.mutex.Lock()
	defer pipelines.mutex.Unlock()
	pipelines.running[pipeline.Id] = pipeline
}

func removeRunningPipeline(id string) {
	pipelines.mutex.Lock()
	defer pipelines.mutex.Unlock()
	delete(pipelines.running, id)
}

func Parse(reader io.Reader) ([]*SpotifySong, error) {
	records, err := csv.NewReader(reader).ReadAll()
	if err != nil {
		return nil, err
	}
	songs := make([]*SpotifySong, 0)
	for _, rr := range records[0] {
		log.Info("Record %s", rr)
	}
	for _, r := range records[1:] {
		if len(r) < 2 {
			log.Warn("Malformed record", r)
		} else {
			songs = append(songs, &SpotifySong{Name: r[0], Artist: r[1], Album: r[2]})
		}
	}
	log.Info("len %d", len(records))
	return songs, nil
}

func Process(context *echo.Context, songs []*SpotifySong) (*PipelineState, error) {
	state := &PipelineState{}
	state.Id = token.RandString(16)
	state.Stats.ImportSize = len(songs)
	state.progressSubscribers = make([]chan *ProgressMessage, 0)
	in := make(chan *SpotifySong, concurrency)
	lookupOut := make(chan *SpotifySong, concurrency)
	done := make(chan bool)
	state.ProcessedSongs = make(chan *SpotifySong)
	for i := 0; i < concurrency; i++ {
		go asyncSearchSpotify(in, lookupOut, done, context)
	}
	go progressUpdater(state, lookupOut)
	go func() {
		for _, song := range songs {
			in <- song
		}
		close(in)
	}()
	go func() {
		for i := 0; i < concurrency; i++ {
			<-done
		}
		close(lookupOut)
	}()
	return state, nil
}

func progressUpdater(state *PipelineState, in <-chan *SpotifySong) {
	stats := &state.Stats
	i := 0
	for song := range in {
		var message ProgressMessage
		if song.SpotifyAlbumUri != "" {
			stats.FoundAlbums++
		} else if song.SpotifyTrackUri != "" {
			stats.FoundTracks++
		} else {
			stats.NotFound++
			log.Info("Not found %+v", song)
			message.NotFoundSong = song
		}
		stats.TotalFound = stats.FoundAlbums + stats.FoundTracks
		stats.ProgressPercent = float64(i) / float64(stats.ImportSize)
		message.Stats = *stats
		subs := state.GetSubscribers()
		for _, sub := range subs {
			sub <- &message
		}
		i++
	}
	for _, sub := range state.GetSubscribers() {
		close(sub)
	}
}

func searchTrack(context *echo.Context, song *SpotifySong) (*SpotifySong, error) {
	v := url.Values{}
	v.Set("type", "track")
	v.Set("q", song.Name+" artist:"+song.Artist)
	reqUrl := searchUrl + "?" + v.Encode()
	resp, err := getWithAuthToken(context, reqUrl, 0)
	if err != nil {
		return song, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return song, err
	}
	if resp.StatusCode != http.StatusOK {
		return song, errors.New("Non-OK http status " + resp.Status)
	}
	tracks := result["tracks"].(map[string]interface{})
	items := tracks["items"].([]interface{})
	if len(items) > 0 {
		itemObj := items[0].(map[string]interface{})
		uri := itemObj["uri"].(string)
		song.SpotifyTrackUri = uri
	}
	return song, nil
}

func searchAlbum(context *echo.Context, song *SpotifySong) (*SpotifySong, error) {
	v := url.Values{}
	v.Set("type", "album")
	v.Set("q", song.Album+" artist:"+song.Artist)
	reqUrl := searchUrl + "?" + v.Encode()
	resp, err := getWithAuthToken(context, reqUrl, 0)
	if err != nil {
		return song, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return song, err
	}
	if resp.StatusCode != http.StatusOK {
		return song, errors.New("Non-OK http status " + resp.Status)
	}
	albums := result["albums"].(map[string]interface{})
	items := albums["items"].([]interface{})
	if len(items) > 0 {
		itemObj := items[0].(map[string]interface{})
		uri := itemObj["uri"].(string)
		song.SpotifyAlbumUri = uri
	}
	return song, nil
}

func asyncSearchSpotify(in <-chan *SpotifySong, out chan<- *SpotifySong, done chan<- bool, context *echo.Context) {
	for song := range in {
		song = searchSpotify(context, song)
		if song.SpotifyTrackUri == "" && song.SpotifyAlbumUri == "" {
			log.Warn("async not found %+v", song)
		}
		out <- song
	}
	done <- true
}

func searchSpotify(context *echo.Context, song *SpotifySong) *SpotifySong {
	song, err := searchAlbum(context, song)
	if err != nil {
		log.Warn("Error looking up album %+v %s", song, err)
	}
	if song.SpotifyAlbumUri == "" {
		song, err = searchTrack(context, song)
		if err != nil {
			log.Warn("Error looking up track %+v %s", song, err)
		}
	}
	return song
}

func getWithAuthToken(context *echo.Context, url string, retries int) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	tokenCookie, err := context.Request().Cookie(config.AccessToken)
	if err != nil {
		return nil, err
	}
	authValue := "Bearer " + tokenCookie.Value
	req.Header.Add("Authorization", authValue)
	resp, err := http.DefaultClient.Do(req)
	if err == nil && resp.StatusCode == 429 && retries < maxRetries {
		// too many requests, retry if we can after a timeout
		// the timeout is in the Retry-After and it is in number of seconds
		timeout, err := strconv.Atoi(resp.Header.Get("Retry-After"))
		if err != nil {
			return nil, err
		}
		resp.Body.Close()
		log.Warn("Retrying after timeout=%d retries=%d", timeout, retries)
		time.Sleep(time.Duration(timeout) * time.Second)
		return getWithAuthToken(context, url, retries+1)
	}
	return resp, err
}
