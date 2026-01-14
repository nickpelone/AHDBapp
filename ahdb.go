// Copyright 2019 MooreaTv moorea@ymail.com
// All Rights Reserved
//
// GPLv3 License (which means no commercial integration)
// ask if you need a different License
//

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"fortio.org/cli"
	"fortio.org/log"
	_ "github.com/mattn/go-sqlite3"
	"github.com/mooreatv/AHDBapp/lua2json"
)

const schemaSql = `
CREATE TABLE IF NOT EXISTS items (
    id TEXT NOT NULL,
    shortid INTEGER NOT NULL,
    name TEXT NOT NULL,
    SellPrice INTEGER NOT NULL,
    StackCount INTEGER NOT NULL,
    ClassID INTEGER NOT NULL,
    SubClassID INTEGER NOT NULL,
    Rarity INTEGER NOT NULL,
    MinLevel INTEGER NOT NULL,
    link TEXT NOT NULL,
    olink TEXT NOT NULL,
    ts TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS scanmeta (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    realm TEXT NOT NULL,
    faction TEXT NOT NULL CHECK(faction IN ('Neutral', 'Alliance', 'Horde')),
    scanner TEXT NOT NULL,
    ts TIMESTAMP NOT NULL,
    CONSTRAINT unique_scan UNIQUE (ts, scanner)
);

CREATE TABLE IF NOT EXISTS auctions (
    scanId INTEGER NOT NULL REFERENCES scanmeta(id),
    itemId TEXT NOT NULL REFERENCES items(id),
    ts TIMESTAMP NOT NULL,
    seller TEXT,
    timeLeft INTEGER NOT NULL,
    itemCount INTEGER NOT NULL,
    minBid INTEGER NOT NULL,
    buyout INTEGER NOT NULL,
    curBid INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS buyoutidx ON auctions (buyout);
CREATE INDEX IF NOT EXISTS nameidx on items (name);
CREATE INDEX IF NOT EXISTS rarityidx on items (rarity);
CREATE INDEX IF NOT EXISTS sellpriceidx on items (sellprice);
CREATE INDEX IF NOT EXISTS itemididx on auctions (itemid);
`

// ScanEntry is 1 auction house scan result.
type ScanEntry struct {
	DataFormatVersion int
	TS                int
	Realm             string
	Faction           string
	Char              string
	Count             int
	ItemDBCount       int
	ItemsCount        int
	Data              string
}

// AHData is toplevel structure produced by ahdbSavedVars2Json.
type AHData struct {
	ItemDB map[string]interface{} `json:"itemDB_2"` // most values are strings except _formatVersion_ and _count_
	Ah     []ScanEntry            `json:"ah"`
}

// ItemEntry is what the raw link gets parsed into.
type ItemEntry struct {
	ID         string
	ShortID    int
	Name       string
	SellPrice  int
	StackCount int
	ClassID    int
	SubClassID int
	Rarity     int
	MinLevel   int
	Link       string
	Olink      string
}

// AuctionEntry is the data we have about each listing.
type AuctionEntry struct {
	TimeLeft  int
	ItemCount int
	MinBid    int
	Buyout    int
	CurBid    int
}

// Re for '5000,1,1,0,1,0|cffffffff|Hitem:14046::::::::5:::::::|h[Runecloth Bag]|h|r'.
var itemRegex = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?),([0-9]+),([0-9]+),([0-9]+),([0-9]+),([0-9]+)(\|[^|]+\|Hitem:([0-9]+)[^|]+\|h\[([^]]+)\]\|h\|r)$`)

func atoi(s string) int {
	if i, err := strconv.Atoi(s); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(f + 0.5)
	}
	return 0
}

func extractItemInfo(id, olink string) *ItemEntry {
	e := ItemEntry{ID: id, Olink: olink}
	if len(olink) == 0 || olink[0] == '|' {
		return &e
	}
	res := itemRegex.FindStringSubmatch(olink)
	if res == nil {
		log.Critf("Unexpected mismatch for item %q", olink)
		return &e
	}
	e.SellPrice = atoi(res[1])
	e.StackCount = atoi(res[2])
	e.ClassID = atoi(res[3])
	e.SubClassID = atoi(res[4])
	e.Rarity = atoi(res[5])
	e.MinLevel = atoi(res[6])
	e.ShortID = atoi(res[8])
	e.Link = res[7]
	e.Name = res[9]
	return &e
}

// Go version of :extractAuctionData() https://github.com/mooreatv/MoLib/blob/v7.11.01/MoLibAH.lua#L437
func extractAuctionData(auction string) AuctionEntry {
	split := strings.Split(auction, ",")
	splitI := make([]int, len(split))
	for i := range split {
		splitI[i] = atoi(split[i])
	}
	return AuctionEntry{TimeLeft: splitI[0], ItemCount: splitI[1], MinBid: splitI[2], Buyout: splitI[3], CurBid: splitI[4]}
}

// Go version of :ahDeserializeScanResult() https://github.com/mooreatv/MoLib/blob/v7.11.01/MoLibAH.lua#L375
func ahDeserializeScanResult(stmt *sql.Stmt, stmtItem *sql.Stmt, scan ScanEntry, scanID int64) error {
	data := scan.Data
	log.LogVf("Deserializing data length %d", len(data))
	numItems := 0
	opCount := 0
	itemEntries := strings.Split(data, " ")
	for itemEntryIdx := range itemEntries {
		itemEntry := itemEntries[itemEntryIdx]
		numItems++
		itemSplit := strings.SplitN(itemEntry, "!", 2)
		if len(itemSplit) != 2 {
			log.Errf("Couldn't split %q into 2 by '!': %#v", itemEntry, itemSplit)
		}
		item := itemSplit[0]
		if stmtItem != nil {
			if _, err := stmtItem.Exec(item); err != nil {
				return fmt.Errorf("failed to ensure item %s exists: %v", item, err)
			}
		}
		rest := itemSplit[1]
		// kr[item] = {}
		// entry := kr[item]
		log.Debugf("for %s rest is '%s'", item, rest)
		bySellerEntries := strings.Split(rest, "!")
		for sellerAuctionsIdx := range bySellerEntries {
			sellerAuctions := bySellerEntries[sellerAuctionsIdx]
			sellerAuctionsSplit := strings.SplitN(sellerAuctions, "/", 2)
			seller := sellerAuctionsSplit[0]
			auctions := strings.Split(sellerAuctionsSplit[1], "&")
			log.Debugf("seller %s auctions are '%#v'", seller, auctions)
			// entry[seller] = {}
			for aIdx := range auctions {
				a := extractAuctionData(auctions[aIdx])
				log.Debugf("Auction %#v", a)
				opCount++
				// scanId, itemId, ts, seller, timeLeft, itemCount, minBid, buyout, curBid)
				if stmt != nil {
					_, err := stmt.Exec(scanID, item, scan.TS, seller, a.TimeLeft, a.ItemCount, a.MinBid, a.Buyout, a.CurBid)
					if err != nil {
						return fmt.Errorf("can't insert in DB op#%d for scanid %d: %v", opCount, scanID, err)
					}
				}
			}
		}
	}
	log.Infof("Inserted %d auctions for %d items for scanId %d", opCount, numItems, scanID)
	if numItems != scan.ItemsCount {
		log.Errf("Mismatch between deserialization item count %d and saved %d", numItems, scan.ItemsCount)
	}
	return nil
}

// SaveScans exports the scan to the DB.
func SaveScans(db *sql.DB, scans []ScanEntry) error {
	stmtMeta := "INSERT INTO scanmeta (realm, faction, scanner, ts) VALUES(?,?,?,datetime(?, 'unixepoch'))"
	var stmtMetaIns *sql.Stmt
	var err error
	if db != nil {
		stmtMetaIns, err = db.Prepare(stmtMeta)
		if err != nil {
			return fmt.Errorf("can't prepare statement for scanmeta insert: %v", err)
		}
	}
	stmtAuction := `
INSERT INTO auctions (scanId, itemId, ts, seller, timeLeft, itemCount, minBid, buyout, curBid)
			 VALUES (?,?, datetime(?, 'unixepoch'), ?,   ?,         ?,        ?,      ?,      ?)
`
	stmtItem := `INSERT OR IGNORE INTO items (id, shortid, name, sellprice, stackcount, classid, subclassid, rarity, minlevel, link, olink)
		VALUES(?, 0, 'Unknown', 0, 0, 0, 0, 0, 0, '', '')`

	for idx := range scans {
		entry := scans[idx]
		if db == nil {
			_ = ahDeserializeScanResult(nil, nil, entry, -1)
			continue
		}
		res, err := stmtMetaIns.Exec(entry.Realm, entry.Faction, entry.Char, entry.TS)
		if err != nil {
			log.Infof("Skipping duplicate entry: %s %d : %v", entry.Char, entry.TS, err)
			continue
		}
		var scanID int64
		if scanID, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("unable to get id after scanmeta insert: %v", err)
		}
		log.LogVf("Inserted successfully scan meta id %d", scanID)
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			return fmt.Errorf("can't start a transaction: %v", err)
		}
		stmtIns, err := tx.Prepare(stmtAuction)
		if err != nil {
			return fmt.Errorf("can't prepare statement for insert: %v", err)
		}
		stmtItemIns, err := tx.Prepare(stmtItem)
		if err != nil {
			return fmt.Errorf("can't prepare statement for item insert: %v", err)
		}
		if err := ahDeserializeScanResult(stmtIns, stmtItemIns, entry, scanID); err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("can't DB commit auction for scan %d: %v", scanID, err)
		}
	}
	// log.Infof("After big commit of all the scans...")
	return nil
}

// SaveItems exports the items to the DB.
func SaveItems(db *sql.DB, items map[string]interface{}) error {
	count := -1
	var stmtIns *sql.Stmt
	var tx *sql.Tx
	var err error
	if db != nil {
		err = db.QueryRow("select count(*) from items").Scan(&count)
		if err != nil {
			return fmt.Errorf("can't count items: %v", err)
		}
		log.Infof("ItemDB at start has %d items", count)
		tx, err = db.BeginTx(context.Background(), nil)
		if err != nil {
			return fmt.Errorf("can't start a transaction: %v", err)
		}
		/* 	this (also) works to conditionally update only if changed (when passed k,v twice but is slower
			stmt := `
		REPLACE INTO items (id, link) select ?,?
			WHERE (SELECT COUNT(*) FROM items WHERE id=? AND link=?) = 0;
		`
		*/
		stmt := `INSERT INTO items (id, shortid, name, sellprice, stackcount, classid, subclassid, rarity, minlevel, link, olink)
							VALUES(?  , ?      , ?   , ?        , ?         , ?      , ?          , ?     , ?       , ?   , ?)
							ON CONFLICT(id) DO UPDATE SET
				ts=CASE WHEN excluded.olink = items.olink THEN items.ts ELSE CURRENT_TIMESTAMP END,
				shortid=excluded.shortid,
				name=excluded.name,
				sellprice=excluded.sellprice,
				stackcount=excluded.stackcount,
				classid=excluded.classid,
				subclassid=excluded.subclassid,
				rarity=excluded.rarity,
				minlevel=excluded.minlevel,
				link=excluded.link,
				olink=excluded.olink`
		stmtIns, err = tx.Prepare(stmt)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("can't prepare statement for insert: %v", err)
		}
		defer stmtIns.Close()
	}
	n := 0
	bytes := 0
	start := time.Now()
	for k, vi := range items {
		v, ok := vi.(string)
		if !ok {
			continue
		}
		lk := len(k)
		if lk == 0 {
			log.Warnf("Invalid empty key %v value %v in itemDB", k, v)
			continue
		}
		bytes = bytes + lk + len(v)
		if db != nil {
			// _, err = stmtIns.Exec(k, v, k, v)
			if k == "_locale_" {
				continue
			}
			e := extractItemInfo(k, v)
			_, err = stmtIns.Exec(e.ID, e.ShortID, e.Name, e.SellPrice, e.StackCount, e.ClassID, e.SubClassID, e.Rarity, e.MinLevel, e.Link, e.Olink)
			if err != nil {
				tx.Rollback()
				return fmt.Errorf("can't insert in DB: %v", err)
			}
		}
		n++
	}
	if db != nil {
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("can't DB commit: %v", err)
		}
		elapsed := time.Since(start)
		log.Infof("Inserted/updated %d items, %.2f Mbytes in DB in %s", n, float64(bytes)/1024./1024., elapsed)
		if err = db.QueryRow("select count(*) from items").Scan(&count); err != nil {
			return fmt.Errorf("can't count items after insert: %v", err)
		}
		log.Infof("ItemDB now has %d items", count)
	} else {
		log.Infof("Parsed %d items, %.2f Mbytes in %s", n, float64(bytes)/1024./1024., time.Since(start))
	}
	return nil
}

// SaveToDB saves items -> db.
func SaveToDB(ahd AHData, noDB bool, outDir string) {
	log.Infof("Starting DB save with noDB=%v ...", noDB)
	var db *sql.DB
	var err error
	var filename string
	fileTime := time.Now()
	if len(ahd.Ah) > 0 {
		// Select the scan with the largest TS
		bestIdx := 0
		maxTS := -1
		for i, s := range ahd.Ah {
			if s.TS > maxTS {
				maxTS = s.TS
				bestIdx = i
			}
		}
		scan := ahd.Ah[bestIdx]
		log.Infof("Selecting scan #%d with largest TS %d (from %d scans)", bestIdx+1, scan.TS, len(ahd.Ah))
		ahd.Ah = []ScanEntry{scan}
		fileTime = time.Unix(int64(maxTS), 0)
	}
	if !noDB {
		filename = fmt.Sprintf("ahdb_%s.db", fileTime.Format("20060102-150405"))
		if outDir != "" {
			if err := os.MkdirAll(outDir, 0755); err != nil {
				log.Fatalf("Can't create output directory %s: %v", outDir, err)
			}
			filename = filepath.Join(outDir, filename)
		}
		if _, err := os.Stat(filename); err == nil {
			log.Infof("File %s already exists, skipping.", filename)
			return
		}
		log.Infof("Opening new SQLite DB: %s", filename)
		db, err = sql.Open("sqlite3", filename)
		if err != nil {
			log.Fatalf("Can't open DB: %v", err)
		}
		defer db.Close()
		if _, err = db.Exec(schemaSql); err != nil {
			db.Close()
			os.Remove(filename)
			log.Fatalf("Can't execute schema: %v", err)
		}
		if _, err = db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
			db.Close()
			os.Remove(filename)
			log.Fatalf("Can't enable foreign keys: %v", err)
		}
	}

	cleanup := func() {
		if db != nil {
			db.Close()
			if err := os.Remove(filename); err != nil {
				log.Errf("Failed to remove %s: %v", filename, err)
			} else {
				log.Infof("Removed incomplete DB file %s", filename)
			}
		}
	}

	if err := SaveItems(db, ahd.ItemDB); err != nil {
		cleanup()
		log.Fatalf("SaveItems failed: %v", err)
	}

	if err := SaveScans(db, ahd.Ah); err != nil {
		cleanup()
		log.Fatalf("SaveScans failed: %v", err)
	}
}

// Go version of :AHGetAuctionInfoByLink() https://github.com/mooreatv/MoLib/blob/v7.11.01/MoLibAH.lua#L86

var (
	jsonOnly = flag.Bool("jsonOnly", false, "Only do the lua to json conversion")
	// BufferSize flag (needs to be big enough for long packed AH scan lines).
	buffSize = flag.Float64("bufferSize", 16, "Buffer size in Mbytes")
	// Whether to skip the top level.
	skipToplevel = flag.Bool("jsonSkipToplevel", false, "Skip top level entity")
	jsonInput    = flag.Bool("jsonInput", false, "Input is already Json and not Lua needing conversion")
	noDB         = flag.Bool("nodb", false, "Don't try to connect to a live DB when the flag is passed")
)

func main() {
	cli.MinArgs = 0
	cli.MaxArgs = 2
	cli.ArgsHelp = " [input_file] [output_dir]"
	cli.Main()
	args := flag.Args()
	var inputReader io.Reader = os.Stdin
	var inputName = "stdin"
	var outDir string

	if len(args) >= 1 {
		inputName = args[0]
		if inputName == "-" {
			inputReader = os.Stdin
			inputName = "stdin"
		} else {
			f, err := os.Open(inputName)
			if err != nil {
				log.Fatalf("Error opening input file %s: %v", inputName, err)
			}
			defer f.Close()
			inputReader = f
		}
	}
	if len(args) >= 2 {
		outDir = args[1]
	}

	if *jsonOnly {
		log.Infof("AHDB lua2json started (reading from %s)...", inputName)
		lua2json.Lua2Json(inputReader, os.Stdout, *skipToplevel, *buffSize)
		return
	}
	log.Infof("AHDB parser started (reading from %s)...", inputName)
	var jR io.Reader
	if *jsonInput {
		jR = inputReader
	} else {
		var jW io.Writer
		jR, jW = io.Pipe()
		go func() {
			lua2json.Lua2Json(inputReader, jW, false /* need to skip to level */, *buffSize)
		}()
	}
	var ahdb AHData
	jdec := json.NewDecoder(jR)
	jdec.UseNumber()

	var generic map[string]interface{}
	if err := jdec.Decode(&generic); err != nil {
		log.Fatalf("Unable to unmarshal json result: %#v", err)
	}
	if val, ok := generic["AuctionDBSaved"]; ok {
		if asMap, ok := val.(map[string]interface{}); ok {
			generic = asMap
			log.Infof("Detected and unwrapped AuctionDBSaved top-level key")
		}
	}
	// Convert back to AHData (using roundtrip marshal/unmarshal for simplicity)
	b, err := json.Marshal(generic)
	if err != nil {
		log.Fatalf("Unable to marshal generic map: %v", err)
	}
	// Use a decoder to unmarshal with UseNumber()
	dec2 := json.NewDecoder(bytes.NewReader(b))
	dec2.UseNumber()
	if err := dec2.Decode(&ahdb); err != nil {
		log.Fatalf("Unable to unmarshal into AHData: %v", err)
	}

	if ahdb.ItemDB == nil {
		for k, v := range generic {
			if strings.HasPrefix(k, "itemDB_") {
				if asMap, ok := v.(map[string]interface{}); ok {
					log.Infof("Found %s, using it as ItemDB", k)
					ahdb.ItemDB = asMap
					break
				}
			}
		}
	}

	fv := ahdb.ItemDB["_formatVersion_"]
	if fv == nil {
		log.Warnf("Missing itemDB format version, assuming 5")
	} else if fv.(json.Number).String() != "5" {
		log.Errf("Unexpected itemDB format version %v", fv)
		os.Exit(1)
	}
	icVal := ahdb.ItemDB["_count_"]
	if icVal == nil {
		log.Warnf("Missing itemDB count (_count_)")
	} else {
		ic, _ := icVal.(json.Number).Int64()
		if int(ic) != len(ahdb.ItemDB)-5 {
			log.Errf("Unexpected itemDB count %v vs %d - 5", icVal, len(ahdb.ItemDB))
		}
	}
	log.Infof("Deserialization done, found %d scans. ItemDB has %d items.", len(ahdb.Ah), len(ahdb.ItemDB)-5) // 4 _ meta keys so far
	SaveToDB(ahdb, *noDB, outDir)
}
