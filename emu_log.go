package main

import (
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type (
	JsonObject map[string]interface{}
	LogRecord  struct {
		date, emuNo, trainNo string
	}
	Bureau struct {
		Code    string
		Name    string
		TrainNo func(this *Bureau, qrCode string) (trainNo, date string, err error)
		Info    func(qrCode string) (info JsonObject, err error)
		Scan    func()
	}
)

var bureaus = []Bureau{
	Bureau{
		Code: "H",
		Name: "中国铁路上海局集团有限公司",
		TrainNo: func(this *Bureau, pqCode string) (trainNo, date string, err error) {
			var info JsonObject
			info, err = this.Info(pqCode)
			if err == nil {
				trainNo = info["trainName"].(string)
				date = time.Now().Format("2006-01-02")
			}
			return
		},
		Info: func(pqCode string) (info JsonObject, err error) {
			const api = "https://g.xiuxiu365.cn/railway_api/web/index/train"
			query := url.Values{"pqCode": {pqCode}}.Encode()
			resp, err := httpClient.Get(api + "?" + query)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			var result struct {
				Code int
				Msg  string
				Data JsonObject
			}
			err = json.NewDecoder(resp.Body).Decode(&result)
			info = result.Data
			return
		},
	},
	Bureau{
		Code: "P",
		Name: "中国铁路北京局集团有限公司",
		TrainNo: func(this *Bureau, qrCode string) (trainNo, date string, err error) {
			var info JsonObject
			info, err = this.Info(qrCode)
			if err == nil {
				trainNo = info["TrainnoId"].(string)
				date = info["TrainnoDate"].(string)
			}
			return
		},
		Info: func(qrCode string) (info JsonObject, err error) {
			const api = "https://aymaoto.jtlf.cn/webapi/otoshopping/ewh_getqrcodetrainnoinfo"
			const key = "qrcode=%s&key=ltRsjkiM8IRbC80Ni1jzU5jiO6pJvbKd"
			sign := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf(key, qrCode))))
			form := url.Values{"qrCode": {qrCode}, "sign": {sign}}
			resp, err := httpClient.PostForm(api, form)
			if err != nil {
				return
			}
			var result struct {
				State int
				Msg   string
				Data  struct {
					TrainInfo JsonObject
					UrlStr    string
				}
			}
			err = json.NewDecoder(resp.Body).Decode(&result)
			info = result.Data.TrainInfo
			return
		},
	},
}

var (
	httpClient = &http.Client{
		Timeout: 5 * time.Second,
	}
	wg sync.WaitGroup
	db *sql.DB
)

const (
	day            = 24 * time.Hour
	repeatInterval = time.Hour
	requestDelay   = 4 * time.Second
	startTime      = 5 * time.Hour
	endTime        = 24 * time.Hour
)

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	checkLocalTimezone()
	checkInternetConnection()
	checkDatabase()

	var nextRun time.Time
	for {
		now := time.Now()
		today := time.Date(
			now.Year(), now.Month(), now.Day(),
			0, 0, 0, 0, time.Local,
		)
		if now.Before(today.Add(startTime)) {
			nextRun = today.Add(startTime)
		} else if now.After(today.Add(endTime)) {
			nextRun = today.Add(day)
		} else {
			nextRun = now.Truncate(repeatInterval).Add(repeatInterval)
		}
		iterBureaus()
		log.Info().Msgf("next schduled run: %v", nextRun)
		time.Sleep(time.Until(nextRun))
	}
}

func iterBureaus() {
	for i := range bureaus {
		wg.Add(1)
		go bureaus[i].iterVehicles()
	}
	wg.Wait()
}

func (b *Bureau) iterVehicles() {
	log.Info().Msgf("job started: %s", b.Name)
	defer wg.Done()

	rows, err := db.Query(`
		SELECT emu_no, emu_qrcode, MIN(rowid)
		FROM emu_qrcode
		WHERE emu_bureau = ?
		GROUP BY emu_no
		ORDER BY emu_no ASC;
	`, b.Code)
	checkFatal(err)
	defer rows.Close()

	for rows.Next() {
		var emuNo, qrCode, id string
		checkFatal(rows.Scan(&emuNo, &qrCode, &id))
		time.Sleep(requestDelay)
		trainNo, date, _ := b.TrainNo(b, qrCode)
		log.Debug().Msgf("%s: %s/%s", emuNo, b.Code, trainNo)
		if trainNo != "" {
			_, err := db.Exec(
				`INSERT OR IGNORE INTO emu_log VALUES (?, ?, ?)`,
				date, emuNo, trainNo,
			)
			checkFatal(err)
		}
	}
	log.Info().Msgf("job done: %s", b.Name)
}

func checkFatal(err error) {
	if err != nil {
		log.Fatal().Msg(err.Error())
	}
}

func checkLocalTimezone() {
	tzName, tzOffset := time.Now().Zone()
	if tzOffset*int(time.Second) != 8*int(time.Hour) {
		log.Warn().Msgf(
			"expected Beijing Timezone (UTC+08), but found %s (UTC%s)",
			tzName, time.Now().Format("-07"),
		)
	}
}

func checkInternetConnection() {
	start := time.Now()
	_, err := bureaus[0].Info("PQ0123456")
	checkFatal(err)
	log.Info().Msgf(
		"internet connection ok, round-trip delay %v",
		time.Since(start),
	)
}

func checkDatabase() {
	dbConn, err := sql.Open("sqlite3", "./emu_log.db")
	checkFatal(err)
	db = dbConn
	// TODO: defer db.Close()

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS emu_log (
		date        VARCHAR NOT NULL,
		emu_no      VARCHAR NOT NULL,
		train_no    VARCHAR NOT NULL,
		UNIQUE(date, emu_no, train_no)
	);`)
	checkFatal(err)
	log.Info().Msgf(
		"found %d log records in database",
		countRecords("emu_log"),
	)

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS emu_qrcode (
		emu_no      VARCHAR NOT NULL,
		emu_bureau  CHAR(1) NOT NULL,
		emu_qrcode  VARCHAR NOT NULL,
		UNIQUE(emu_bureau, emu_qrcode)
	);`)
	checkFatal(err)
	log.Info().Msgf(
		"found %d qr code records in database",
		countRecords("emu_qrcode"),
	)
}

func countRecords(tableName string) (count int) {
	row := db.QueryRow(`SELECT COUNT(*) FROM ` + tableName)
	err := row.Scan(&count)
	checkFatal(err)
	return
}