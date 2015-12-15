package config

import (
	"encoding/json"
	log "github.com/Sirupsen/logrus"
	"os"
)

const (
	AccessToken  = "access_token"
	RefreshToken = "refresh_token"
)

type Config struct {
	ClientID       string `json:"clientId"`
	ClientSecret   string `json:"clientSecret"`
	OAuthCallback  string `json:"oauthCallback"`
	RedirectPrefix string `json:"redirectPrefix"`
}

var config Config

func GetConfig() Config {
	return config
}

func LoadConfig(string string) error {
	configFile, err := os.Open("config.json")
	defer configFile.Close()
	if err != nil {
		return err
	}

	err = json.NewDecoder(configFile).Decode(&config)
	if err != nil {
		return err
	}
	log.Infof("config = %+v", config)
	return nil
}
