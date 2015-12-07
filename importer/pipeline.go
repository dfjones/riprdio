package importer

import (
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
	"strings"
	"runtime"
	"io/ioutil"
)

const (
	defaultContentType = "text/csv"
	searchUrl   = "https://api.spotify.com/v1/search"
	albumUrl    = "https://api.spotify.com/v1/me/albums"
	trackUrl    = "https://api.spotify.com/v1/me/tracks"
	concurrency = 3
	maxRetries  = 50
	batchSize = 50
)

var (
	pipelines = pipelineStates{sync.Mutex{}, make(map[string]*PipelineState)}
	formats []*format
)

type format struct {
	name string
	contentType string
	parse Parse
}

type Parse func(reader io.Reader) ([]*SpotifySong, error)

type SpotifySong struct {
	Name           string
	Artist         string
	Album          string
	SpotifyAlbumId string
	SpotifyTrackId string
	ImportError    error
}

type pipelineStates struct {
	mutex   sync.Mutex
	running map[string]*PipelineState
}

type PipelineState struct {
	Id                  string
	ProcessedSongs      chan *SpotifySong
	Stats               PipelineStats
	StartTime			time.Time
	mx                  sync.Mutex
	progressSubscribers []chan *ProgressMessage
	albumIdCache		map[string]string
}

type PipelineStats struct {
	ImportSize      int
	FoundAlbums     int
	FoundTracks     int
	TotalFound      int
	NotFound        int
	Errors          int
	ProgressPercent float64
}

type ProgressMessage struct {
	Stats        PipelineStats
	NotFoundSong *SpotifySong
}

type searchJson map[string]interface{}

func init() {
	go func() {
		for _ = range time.Tick(time.Minute) {
			snapshot := getRunningPipelineSnapshot()
			log.Info("Goroutines=%d", runtime.NumGoroutine())
			log.Info("Running pipelines %d", len(snapshot))
			for _,state := range pipelines.running {
				state.mx.Lock()
				log.Info("Pipeline id %s progress %2.1f", state.Id, state.Stats.ProgressPercent)
				state.mx.Unlock()
			}
		}
	}()
}

func RegisterFormat(name, contentType string, parser Parse) {
	formats = append(formats, &format{name, contentType, parser})
}

func selectFormat(contentType string) *format {
	var defaultFormat *format
	for _,f := range formats {
		if f.contentType == defaultContentType {
			defaultFormat = f
		}
		if f.contentType == contentType {
			return f
		}
	}
	// no match...CSV was the original format supported, so select that
	return defaultFormat
}

func getRunningPipelineSnapshot() []*PipelineState {
	states := make([]*PipelineState, 0)
	pipelines.mutex.Lock()
	for _,s := range pipelines.running {
		states = append(states, s)
	}
	pipelines.mutex.Unlock()
	return states
}

func (p *PipelineState) CreateSubscriber() chan *ProgressMessage {
	c := make(chan *ProgressMessage)
	p.mx.Lock()
	defer p.mx.Unlock()
	p.progressSubscribers = append(p.progressSubscribers, c)
	log.Info("Created subscriber for pipeline: %s", p.Id)
	return c
}

func (p *PipelineState) RemoveSubscriber(s chan *ProgressMessage) {
	p.mx.Lock()
	defer p.mx.Unlock()
	for i, c := range p.progressSubscribers {
		if c == s {
			p.progressSubscribers = append(p.progressSubscribers[:i], p.progressSubscribers[i+1:]...)
			log.Info("Removed subscriber for pipeline: %s", p.Id)
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

func (p *PipelineState) CacheAlbumId(album string, artist string, albumId string) {
	key := makeAlbumCacheKey(album, artist)
	p.mx.Lock()
	defer p.mx.Unlock()
	p.albumIdCache[key] = albumId
}

func (p *PipelineState) GetCachedAlbumId(album string, artist string) (string, bool) {
	key := makeAlbumCacheKey(album, artist)
	p.mx.Lock()
	defer p.mx.Unlock()
	v, ok := p.albumIdCache[key]
	return v, ok
}

func makeAlbumCacheKey(album string, artist string) string {
	return album + "-" + artist
}

func RunImportPipeline(context *echo.Context, contentType string, reader io.Reader) (*PipelineState, error) {
	format := selectFormat(contentType)
	if format == nil {
		log.Warn("No format found for content type %s", contentType)
		return nil, errors.New("No matching content type")
	}
	log.Info("Selected format %s for content type %s", format.name, contentType)
	songs, err := format.parse(reader)
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

func newPipelineState(importSize int) *PipelineState {
	state := &PipelineState{}
	state.Id = token.RandString(16)
	state.ProcessedSongs = make(chan *SpotifySong)
	state.progressSubscribers = make([]chan *ProgressMessage, 0)
	state.Stats.ImportSize = importSize
	state.albumIdCache = make(map[string]string)
	state.StartTime = time.Now()
	return state
}

func Process(context *echo.Context, songs []*SpotifySong) (*PipelineState, error) {
	state := newPipelineState(len(songs))
	in := make(chan *SpotifySong, concurrency)
	importOut := make(chan *SpotifySong, concurrency)
	done := make(chan bool)
	for i := 0; i < concurrency; i++ {
		go asyncImportSpotify(in, importOut, done, context, state)
	}
	go progressUpdater(state, importOut)
	log.Info("Starting import of %d songs for id=%s", len(songs), state.Id)
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
		close(importOut)
	}()
	return state, nil
}

func progressUpdater(state *PipelineState, in <-chan *SpotifySong) {
	stats := &state.Stats
	i := 1
	for song := range in {
		var message ProgressMessage
		if song.SpotifyAlbumId != "" {
			stats.FoundAlbums++
		} else if song.SpotifyTrackId != "" {
			stats.FoundTracks++
		} else {
			stats.NotFound++
			message.NotFoundSong = song
		}
		if song.ImportError != nil {
			stats.Errors++
		}
		state.mx.Lock()
		stats.TotalFound = stats.FoundAlbums + stats.FoundTracks
		stats.ProgressPercent = 100.0 * float64(i) / float64(stats.ImportSize)
		state.mx.Unlock()
		message.Stats = *stats
		i++
		subs := state.GetSubscribers()
		for _, sub := range subs {
			sub <- &message
		}
	}
	for _, sub := range state.GetSubscribers() {
		close(sub)
	}
	removeRunningPipeline(state.Id)
	delta := time.Since(state.StartTime)
	log.Info("Finished import id=%s imported=%d of total songs=%d time=%s", state.Id, stats.TotalFound,
		stats.ImportSize, delta.String())
}

func searchTrack(context *echo.Context, song *SpotifySong) (*SpotifySong, error) {
	v := url.Values{}
	v.Set("type", "track")
	v.Set("q", song.Name+" artist:"+song.Artist)
	reqUrl := searchUrl + "?" + v.Encode()
	resp, err := getWithAuthToken(context, reqUrl)
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
		id := itemObj["id"].(string)
		song.SpotifyTrackId = id
	}
	return song, nil
}

func searchAlbum(context *echo.Context, p *PipelineState, song *SpotifySong) (*SpotifySong, error) {
	if cachedId, ok := p.GetCachedAlbumId(song.Album, song.Artist); ok {
		song.SpotifyAlbumId = cachedId
		return song, nil
	}
	v := url.Values{}
	v.Set("type", "album")
	v.Set("q", song.Album+" artist:"+song.Artist)
	reqUrl := searchUrl + "?" + v.Encode()
	resp, err := getWithAuthToken(context, reqUrl)
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
		id := itemObj["id"].(string)
		song.SpotifyAlbumId = id
		p.CacheAlbumId(song.Album, song.Artist, id)
	}
	return song, nil
}

func asyncImportSpotify(in <-chan *SpotifySong, out chan<- *SpotifySong, done chan<- bool, context *echo.Context, p *PipelineState) {
	batch := make(chan *SpotifySong)

	go asyncBatchImport(context, batch, done, out)

	for song := range in {
		song = searchSpotify(context, p, song)
		batch <- song
	}

	close(batch)
}

func asyncBatchImport(context *echo.Context, in <-chan *SpotifySong, done chan<- bool, out chan<- *SpotifySong) {
	batch := make([]*SpotifySong, 0, batchSize)

	runBatch := func() {
		err := importSpotify(context, batch)
		for _,song := range batch {
			song.ImportError = err
			out <- song
		}
		batch = make([]*SpotifySong, 0, batchSize)
	}

	for song := range in {
		batch = append(batch, song)
		if len(batch) >= batchSize {
			runBatch()
		}
	}

	runBatch()
	done <- true
}

func importSpotify(context *echo.Context, songs []*SpotifySong) error {
	albumIdSet:= make(map[string]bool)
	trackIds := make([]string, 0)
	toPut := make(map[string][]string)

	for _,song := range songs {
		if song.SpotifyAlbumId != "" {
			albumIdSet[song.SpotifyAlbumId] = true
		} else if song.SpotifyTrackId != "" {
			trackIds = append(trackIds, song.SpotifyTrackId)
		}
	}

	albumIds := make([]string, 0, len(albumIdSet))
	for albumId, _ := range albumIdSet {
		albumIds = append(albumIds, albumId)
	}

	toPut[albumUrl] = albumIds
	toPut[trackUrl] = trackIds

	for putUrl, ids := range toPut {
		if len(ids) == 0 {
			continue
		}
		v := url.Values{}
		v.Set("ids", strings.Join(ids, ","))
		if putUrl != "" {
			resp, err := putWithAuthToken(context, putUrl + "?" + v.Encode())
			if err != nil {
				log.Error("Error importing song batch to url %s %s", putUrl, err)
				return err
			}
			if resp.StatusCode != http.StatusOK {
				body, _ := ioutil.ReadAll(resp.Body)
				log.Error("Non-OK Status from API for %s: %s \n %s", putUrl, resp.Status, body)
				return errors.New("Add song/album API returned status" + resp.Status)
			}
		}
	}

	return nil
}

func searchSpotify(context *echo.Context, p *PipelineState, song *SpotifySong) *SpotifySong {
	song, err := searchAlbum(context, p, song)
	if err != nil {
		log.Warn("Error looking up album %+v %s", song, err)
	}
	if song.SpotifyAlbumId == "" {
		song, err = searchTrack(context, song)
		if err != nil {
			log.Warn("Error looking up track %+v %s", song, err)
		}
	}
	return song
}

func putWithAuthToken(context *echo.Context, url string) (*http.Response, error) {
	r := func() (*http.Request, error) {
		req, err := http.NewRequest("PUT", url, nil)
		if err != nil {
			return nil, err
		}
		err = addAuthToken(context, req)
		if err != nil {
			return nil, err
		}
		return req, nil
	}
	return doWithRetry(r, 0)
}

func getWithAuthToken(context *echo.Context, url string) (*http.Response, error) {
	r := func() (*http.Request, error) {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
		err = addAuthToken(context, req)
		if err != nil {
			return nil, err
		}
		return req, nil
	}
	return doWithRetry(r, 0)
}

func addAuthToken(context *echo.Context, req *http.Request) error {
	tokenCookie, err := context.Request().Cookie(config.AccessToken)
	if err != nil {
		return err
	}
	authValue := "Bearer " + tokenCookie.Value
	req.Header.Add("Authorization", authValue)
	return nil
}

type requestSupplier func() (*http.Request, error)

func doWithRetry(reqS requestSupplier, retries int) (*http.Response, error) {
	req, err := reqS()
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil && resp.StatusCode == 429 {
		if retries < maxRetries {
			// too many requests, retry if we can after a timeout
			// the timeout is in the Retry-After and it is in number of seconds
			timeout, err := strconv.Atoi(resp.Header.Get("Retry-After"))
			if err != nil {
				return nil, err
			}
			resp.Body.Close()
			time.Sleep(time.Duration(timeout) * time.Second)
			return doWithRetry(reqS, retries+1)
		} else {
			log.Warn("Too many retries. Giving up %s", req.URL.String())
			return resp, err
		}
	}
	return resp, err
}
