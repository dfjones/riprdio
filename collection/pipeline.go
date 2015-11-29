package collection

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"github.com/dfjones/riprdio/config"
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

type spotifySong struct {
	name            string
	artist          string
	album           string
	spotifyAlbumUri string
	spotifyTrackUri string
}

type searchJson map[string]interface{}

func RunImportPipeline(context *echo.Context, reader io.Reader) error {
	songs, err := Parse(reader)
	_, err = Lookup(context, songs)
	return err
}

func Parse(reader io.Reader) ([]spotifySong, error) {
	records, err := csv.NewReader(reader).ReadAll()
	if err != nil {
		return nil, err
	}
	songs := make([]spotifySong, 0)
	for _, rr := range records[0] {
		log.Info("Record %s", rr)
	}
	for _, r := range records[1:] {
		if len(r) < 2 {
			log.Warn("Malformed record", r)
		} else {
			songs = append(songs, spotifySong{name: r[0], artist: r[1], album: r[2]})
		}
	}
	log.Info("len %d", len(records))
	return songs, nil
}

func Lookup(context *echo.Context, songs []spotifySong) ([]spotifySong, error) {
	slen := len(songs)
	in := make(chan spotifySong, concurrency)
	lookupOut := make(chan spotifySong, concurrency)
	loggerOut := make(chan spotifySong)
	for i := 0; i < concurrency; i++ {
		go asyncSearchSpotify(in, lookupOut, context)
	}
	go progressLogger(slen, lookupOut, loggerOut)
	wg := sync.WaitGroup{}
	wg.Add(1)
	var finishedSongs []spotifySong
	go func() {
		processedSongs := make([]spotifySong, 0)
		for i := 0; i < slen; i++ {
			song := <-loggerOut
			processedSongs = append(processedSongs, song)
		}
		close(lookupOut)
		finishedSongs = processedSongs
		wg.Done()
	}()

	for _, song := range songs {
		in <- song
	}
	close(in)

	wg.Wait()
	return finishedSongs, nil
}

func progressLogger(totalLen int, in <-chan spotifySong, out chan<- spotifySong) {
	foundAlbum := 0
	foundTrack := 0
	notFound := 0
	i := 0
	for song := range in {
		log.Info("foundAlbum=%d foundTrack=%d totalFound=%d notFound=%d percent=%2.1f", foundAlbum, foundTrack, foundAlbum+foundTrack, notFound, float64(i)/float64(totalLen)*100.0)
		if song.spotifyAlbumUri != "" {
			foundAlbum++
		} else if song.spotifyTrackUri != "" {
			foundTrack++
		} else {
			notFound++
			log.Info("Not found %+v", song)
		}
		i++
		out <- song
	}
	log.Info("finished totalFound=%d foundAlbum=%d foundTrack=%d notFound=%d total=%d", foundAlbum+foundTrack, foundAlbum, foundTrack, notFound, totalLen)
}

func searchTrack(context *echo.Context, song spotifySong) (spotifySong, error) {
	v := url.Values{}
	v.Set("type", "track")
	v.Set("q", song.name+" artist:"+song.artist)
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
		song.spotifyTrackUri = uri
	}
	return song, nil
}

func searchAlbum(context *echo.Context, song spotifySong) (spotifySong, error) {
	v := url.Values{}
	v.Set("type", "album")
	v.Set("q", song.album+" artist:"+song.artist)
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
		song.spotifyAlbumUri = uri
	}
	return song, nil
}

func asyncSearchSpotify(in <-chan spotifySong, out chan<- spotifySong, context *echo.Context) {
	for song := range in {
		song = searchSpotify(context, song)
		if song.spotifyTrackUri == "" && song.spotifyAlbumUri == "" {
			log.Warn("async not found %+v", song)
		}
		out <- song
	}
}

func searchSpotify(context *echo.Context, song spotifySong) spotifySong {
	song, err := searchAlbum(context, song)
	if err != nil {
		log.Warn("Error looking up album %+v %s", song, err)
	}
	if song.spotifyAlbumUri == "" {
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
