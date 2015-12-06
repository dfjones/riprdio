package json

import (
	"os"
	"testing"
)

func TestParse(t *testing.T) {
	file, err := os.Open("test.json")
	if err != nil {
		t.Error("Error opening test json file", err)
	}
	songs, err := Parse(file)
	if err != nil {
		t.Error("Error during parsing", err)
	}
	if len(songs) != 2 {
		t.Errorf("Song length was %d, expected 2", len(songs))
	}
	s1 := songs[0]
	if s1.Name != "Remind Me" {
		t.Errorf("Expected Song Name Remind Me but got %s", s1.Name)
	}
	if s1.Album != "Melody AM" {
		t.Errorf("Expected Album Melody AM but got %s", s1.Album)
	}
	if s1.Artist != "Röyksopp" {
		t.Errorf("Expected Artist Röyksopp but got %s", s1.Artist)
	}

	s2 := songs[1]
	if s2.Name != "Disc Wars" {
		t.Errorf("Expected Song Name Disc Wars but got %s", s2.Name)
	}
	if s2.Album != "TRON: Legacy" {
		t.Errorf("Expected Album TRON: Legacy but got %s", s2.Album)
	}
	if s2.Artist != "Daft Punk" {
		t.Errorf("Expected Artist Daft Punk but got %s", s2.Artist)
	}
}