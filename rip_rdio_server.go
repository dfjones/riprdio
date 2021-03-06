package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/GeertJohan/go.rice"
	log "github.com/Sirupsen/logrus"
	logrus "github.com/deoxxa/echo-logrus"
	"github.com/dfjones/riprdio/config"
	"github.com/dfjones/riprdio/importer"
	_ "github.com/dfjones/riprdio/importer/csv"  // add csv format
	_ "github.com/dfjones/riprdio/importer/json" // add json format
	"github.com/dfjones/riprdio/token"
	"github.com/labstack/echo"
	mw "github.com/labstack/echo/middleware"
	"net/http"
	"net/url"
)

type AuthData struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

var (
	conf config.Config
)

const (
	scope           = "user-read-private user-read-email playlist-read-private playlist-modify-public playlist-modify-private user-library-read user-library-modify playlist-read-collaborative"
	spotifyTokenUrl = "https://accounts.spotify.com/api/token"
	redirectUri     = "http://localhost:3030/callback"
	stateCookieKey  = "spotify_auth_state"
)

func main() {
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true, ForceColors: true})
	config.LoadConfig("config.json")
	conf = config.GetConfig()

	e := echo.New()

	e.Use(logrus.New())
	e.Use(mw.Recover())

	assetHandler := http.FileServer(rice.MustFindBox("public").HTTPBox())
	e.Get("/", func(c *echo.Context) error {
		assetHandler.ServeHTTP(c.Response().Writer(), c.Request())
		return nil
	})

	e.Get("/static*", func(c *echo.Context) error {
		http.StripPrefix("/static/", assetHandler).
			ServeHTTP(c.Response().Writer(), c.Request())
		return nil
	})

	e.Get("/login", func(c *echo.Context) error {
		state := token.RandString(16)
		log.Infof("state %s", state)
		resp := c.Response()
		http.SetCookie(resp.Writer(), &http.Cookie{Name: stateCookieKey, Value: state})
		v := url.Values{}
		v.Set("response_type", "code")
		v.Set("client_id", conf.ClientID)
		v.Set("scope", scope)
		ruri := conf.OAuthCallback
		if ruri == "" {
			ruri = redirectUri
		}
		v.Set("redirect_uri", ruri)
		v.Set("state", state)
		rUri := "https://accounts.spotify.com/authorize?" + v.Encode()
		return c.Redirect(http.StatusFound, rUri)
	})

	e.Get("/callback", func(c *echo.Context) error {
		req := c.Request()
		code := c.Query("code")
		state := c.Query("state")
		log.Infof("code %s state %s", code, state)
		storedState, err := req.Cookie(stateCookieKey)
		if err != nil {
			return err
		}
		if state == "" || state != storedState.Value {
			v := url.Values{}
			v.Set("error", "state_mismatch")
			return c.Redirect(http.StatusFound, "/#"+v.Encode())
		}

		storedState.Value = ""
		http.SetCookie(c.Response().Writer(), storedState)

		ruri := conf.OAuthCallback
		if ruri == "" {
			ruri = redirectUri
		}
		v := url.Values{}
		v.Set("code", code)
		v.Set("redirect_uri", ruri)
		v.Set("grant_type", "authorization_code")
		v.Set("client_id", conf.ClientID)
		v.Set("client_secret", conf.ClientSecret)

		authResp, err := http.PostForm(spotifyTokenUrl, v)
		if err != nil {
			log.Errorf("Error requesting token", err)
			return err
		}
		defer authResp.Body.Close()
		log.Infof("status = %s", authResp.Status)

		var authData AuthData
		err = json.NewDecoder(authResp.Body).Decode(&authData)
		if err != nil {
			log.Errorf("err decoding json", err)
			return err
		}
		log.Infof("data %+v", authData)

		resp := c.Response()
		http.SetCookie(resp.Writer(), &http.Cookie{Name: config.AccessToken, Value: authData.AccessToken})
		http.SetCookie(resp.Writer(), &http.Cookie{Name: config.RefreshToken, Value: authData.RefreshToken})
		rv := url.Values{}
		rv.Set(config.AccessToken, authData.AccessToken)
		rv.Set(config.RefreshToken, authData.RefreshToken)
		return c.Redirect(http.StatusFound, conf.RedirectPrefix+"/#"+rv.Encode())
	})

	e.Get("/refresh_token", func(c *echo.Context) error {
		refreshToken := c.Query("refresh_token")
		v := url.Values{}
		v.Set("client_id", conf.ClientID)
		v.Set("client_secret", conf.ClientSecret)
		v.Set("grant_type", "refresh_token")
		v.Set("refresh_token", refreshToken)

		resp, err := http.PostForm(spotifyTokenUrl, v)
		if err != nil {
			return err
		}
		var authData AuthData
		err = json.NewDecoder(resp.Body).Decode(&authData)
		if err != nil {
			return err
		}
		http.SetCookie(c.Response().Writer(), &http.Cookie{Name: config.AccessToken, Value: authData.AccessToken})
		return c.JSON(http.StatusOK, authData)
	})

	e.Post("/upload", func(c *echo.Context) error {
		mr, err := c.Request().MultipartReader()
		if err != nil {
			return err
		}
		part, err := mr.NextPart()
		if err != nil {
			return err
		}
		defer part.Close()
		log.Infof("%+v", part)
		contentType := part.Header.Get("Content-Type")
		log.Infof("Upload with content-type %s", contentType)
		state, err := importer.RunImportPipeline(c, contentType, part)
		if err != nil {
			return err
		}
		accessToken, err := c.Request().Cookie(config.AccessToken)
		if err != nil {
			return err
		}
		refreshToken, err := c.Request().Cookie(config.RefreshToken)
		if err != nil {
			return err
		}
		v := url.Values{}
		v.Set("pipeline_id", state.Id)
		v.Set(config.AccessToken, accessToken.Value)
		v.Set(config.RefreshToken, refreshToken.Value)
		return c.Redirect(http.StatusFound, conf.RedirectPrefix+"#"+v.Encode())
	})

	e.Get("/progress/:id", func(c *echo.Context) error {
		writer := c.Response().Writer()
		flusher, ok := c.Response().Writer().(http.Flusher)
		if !ok {
			return errors.New("Streaming unsupported")
		}
		header := c.Response().Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("Connection", "keep-alive")

		id := c.Param("id")
		log.Infof("Looking up pipeline %s", id)
		pipeline := importer.GetRunningPipeline(id)
		if pipeline == nil {
			return c.NoContent(http.StatusNotFound)
		}

		defer log.Infof("progress method return for id %s", id)

		sub := pipeline.CreateSubscriber()
		defer pipeline.RemoveSubscriber(sub)

		for message := range sub {
			statsJson, err := json.Marshal(message.Stats)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(writer, "event: progress\ndata: %s\n\n", statsJson)
			if err != nil {
				return err
			}
			if message.NotFoundSong != nil {
				songJson, err := json.Marshal(message.NotFoundSong)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(writer, "event: notfound\ndata: %s\n\n", songJson)
				if err != nil {
					return err
				}
			}
			flusher.Flush()
		}

		_, err := fmt.Fprintf(writer, "event: end\ndata:%s\n\n", `{"end": true}`)
		if err != nil {
			return err
		}
		flusher.Flush()
		log.Infof("Wrote end event to client.")

		return nil
	})

	e.Run(":3030")
}
