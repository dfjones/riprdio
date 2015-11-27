package main

import (
	"encoding/json"
	"github.com/GeertJohan/go.rice"
	"github.com/labstack/echo"
	mw "github.com/labstack/echo/middleware"
	"github.com/labstack/gommon/log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"time"
)

type Config struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

type AuthData struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

var (
	runes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
)

const (
	spotifyTokenUrl = "https://accounts.spotify.com/api/token"
	redirectUri     = "http://localhost:3000/callback"
	stateCookieKey  = "spotify_auth_state"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

func randString(n int) string {
	r := make([]rune, n)
	for i := range r {
		r[i] = runes[rand.Intn(len(runes))]
	}
	return string(r)
}

func main() {
	var config Config

	configFile, err := os.Open("config.json")
	if err != nil {
		log.Fatal(err)
	}

	err = json.NewDecoder(configFile).Decode(&config)
	if err != nil {
		log.Fatal(err)
	}
	log.Info("config = %+v", config)
	configFile.Close()

	e := echo.New()

	e.Use(mw.Logger())
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
		state := randString(16)
		log.Info("state %s", state)
		resp := c.Response()
		http.SetCookie(resp.Writer(), &http.Cookie{Name: stateCookieKey, Value: state})
		scope := "user-read-private user-read-email"
		v := url.Values{}
		v.Set("response_type", "code")
		v.Set("client_id", config.ClientID)
		v.Set("scope", scope)
		v.Set("redirect_uri", redirectUri)
		v.Set("state", state)
		rUri := "https://accounts.spotify.com/authorize?" + v.Encode()
		return c.Redirect(http.StatusFound, rUri)
	})

	e.Get("/callback", func(c *echo.Context) error {
		req := c.Request()
		code := c.Query("code")
		state := c.Query("state")
		log.Info("code %s state %s", code, state)
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

		v := url.Values{}
		v.Set("code", code)
		v.Set("redirect_uri", redirectUri)
		v.Set("grant_type", "authorization_code")
		v.Set("client_id", config.ClientID)
		v.Set("client_secret", config.ClientSecret)

		authResp, err := http.PostForm(spotifyTokenUrl, v)
		if err != nil {
			log.Error("Error requesting token", err)
			return err
		}
		defer authResp.Body.Close()
		log.Info("status = %s", authResp.Status)

		var authData AuthData
		err = json.NewDecoder(authResp.Body).Decode(&authData)
		if err != nil {
			log.Error("err decoding json", err)
			return err
		}
		log.Info("data %+v", authData)

		resp := c.Response()
		http.SetCookie(resp.Writer(), &http.Cookie{Name: "access_token", Value: authData.AccessToken})
		http.SetCookie(resp.Writer(), &http.Cookie{Name: "refresh_token", Value: authData.RefreshToken})
		rv := url.Values{}
		rv.Set("access_token", authData.AccessToken)
		rv.Set("refresh_token", authData.RefreshToken)
		return c.Redirect(http.StatusFound, "/#"+rv.Encode())
	})

	e.Get("/refresh_token", func(c *echo.Context) error {
		refreshToken := c.Query("refresh_token")
		v := url.Values{}
		v.Set("client_id", config.ClientID)
		v.Set("client_secret", config.ClientSecret)
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
		http.SetCookie(c.Response().Writer(), &http.Cookie{Name: "access_token", Value: authData.AccessToken})
		return c.JSON(http.StatusOK, authData)
	})

	e.Run(":3000")
}
