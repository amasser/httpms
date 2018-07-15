package library

import (
	"bytes"
	"database/sql"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// FindAndSaveAlbumArtwork implements the ArtworkFinder interface for the local library.
// It would return a previously found artwork if any or try to find one in the
// filesystem or _on the internet_! This function returns ReadCloser and the caller
// is resposible for freeing the used resources by calling Close().
//
// When an artwork is found it will be saved in the database and once there it will be
// served from the db. Wait, wait! Serving binary files from the database?! Isn't that
// slow? Apparantly no with sqlite3. See the following:
//
// * https://www.sqlite.org/intern-v-extern-blob.html
// * https://www.sqlite.org/fasterthanfs.html
//
// This behaviour have an additional bonus that artwork found on the internet will not
// be saved on the filesystem and thus "pollute" it with unexpected files. It will be
// nicely contained in the app's database.
//
// !TODO: Make sure there is no race conditions while getting/saving artwork for
// particular album.
func (lib *LocalLibrary) FindAndSaveAlbumArtwork(albumID int64) (io.ReadCloser, error) {
	reader, err := lib.albumArtworkFromDB(albumID)
	if err == ErrCachedArtworkNotFound {
		return nil, ErrArtworkNotFound
	} else if err == nil || err != ErrArtworkNotFound {
		return reader, err
	}

	reader, err = lib.albumArtworkFromFS(albumID)
	if err == nil {
		return lib.saveAlbumArtwork(albumID, reader)
	} else if err != ErrArtworkNotFound {
		return nil, err
	}

	if err := lib.saveAlbumArtworkNotFound(albumID); err != nil {
		return nil, err
	}

	return nil, ErrArtworkNotFound
}

func (lib *LocalLibrary) saveAlbumArtwork(
	albumID int64,
	artwork io.ReadCloser,
) (io.ReadCloser, error) {
	buff, err := ioutil.ReadAll(artwork)
	if err != nil {
		return nil, err
	}

	stmt, err := lib.db.Prepare(`
			INSERT INTO
				albums_artworks (album_id, artwork_cover, updated_at)
			VALUES
				(?, ?, ?)
	`)

	if err != nil {
		return nil, err
	}

	defer stmt.Close()

	_, err = stmt.Exec(albumID, buff, time.Now().Unix())

	if err != nil {
		return nil, err
	}

	return newBytesReadCloser(bytes.NewReader(buff)), nil
}

func (lib *LocalLibrary) saveAlbumArtworkNotFound(albumID int64) error {
	stmt, err := lib.db.Prepare(`
			INSERT INTO
				albums_artworks (album_id, updated_at)
			VALUES
				(?, ?)
	`)

	if err != nil {
		return err
	}

	defer stmt.Close()

	_, err = stmt.Exec(albumID, time.Now().Unix())
	if err != nil {
		return err
	}

	return nil
}

func (lib *LocalLibrary) albumArtworkFromDB(albumID int64) (io.ReadCloser, error) {
	smt, err := lib.db.Prepare(`
		SELECT
			artwork_cover,
			updated_at
		FROM
			albums_artworks
		WHERE
			album_id = ?
	`)

	if err != nil {
		log.Printf("could not prepare album artwork sql statement: %s", err)
		return nil, err
	}
	defer smt.Close()

	var (
		buff     []byte
		unixTime int64
	)

	err = smt.QueryRow(albumID).Scan(&buff, &unixTime)
	if err == sql.ErrNoRows {
		return nil, ErrArtworkNotFound
	} else if err != nil {
		log.Printf("error getting album cover from db: %s", err)
		return nil, err
	}

	if len(buff) < 1 && time.Now().Before(time.Unix(unixTime, 0).Add(24*7*time.Hour)) {
		return nil, ErrCachedArtworkNotFound
	}

	return newBytesReadCloser(bytes.NewReader(buff)), nil
}

func (lib *LocalLibrary) albumArtworkFromFS(albumID int64) (io.ReadCloser, error) {
	albumPath, err := lib.GetAlbumFSPathByID(albumID)

	if err != nil {
		return nil, err
	}

	imagesRegexp := regexp.MustCompile(`(?i).*\.(png|gif|jpeg|jpg)$`)
	var possibleArtworks []string

	err = filepath.Walk(albumPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip directories
			return nil
		}
		if imagesRegexp.MatchString(path) {
			possibleArtworks = append(possibleArtworks, path)
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	if len(possibleArtworks) < 1 {
		return nil, ErrArtworkNotFound
	}

	var (
		selectedArtwork string
		score           int
	)

	for _, path := range possibleArtworks {
		pathScore := 5

		fileBase := strings.ToLower(filepath.Base(path))

		if strings.HasPrefix(fileBase, "cover.") || strings.HasPrefix(fileBase, "front.") {
			pathScore = 15
		}

		if strings.Contains(fileBase, "cover") || strings.Contains(fileBase, "front") {
			pathScore = 10
		}

		if strings.Contains(fileBase, "artwork") {
			pathScore = 8
		}

		if pathScore > score {
			selectedArtwork = path
			score = pathScore
		}
	}

	return os.Open(selectedArtwork)
}
