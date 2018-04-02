package main

import (
    "net/http"
    "fmt"
    "os"
    "os/signal"
    "syscall"
    "encoding/json"
    "io/ioutil"
    "regexp"
    "strconv"
    "errors"
    "sort"
    "time"
    "compress/flate"
    "bytes"
    "crypto/tls"
    "log"
)

const poolsJSON string = "https://raw.githubusercontent.com/turtlecoin/" +
                         "turtlecoin-pools-json/master/turtlecoin-pools.json"

/* The amount of blocks a pool can vary from the others before we notify */
const poolMaxDifference int = 5

/* How often we check the pools */
const poolRefreshRate time.Duration = time.Second * 30

/* The data type we parse our json into */
type Pool struct {
    Api string `json:"url"`
}

/* Map of pool name to pool api */
type Pools map[string]Pool

/* Info about every pool */
type PoolsInfo struct {
    pools               []PoolInfo
    medianHeight        int
    heightLastUpdated   time.Time
}

/* Info about an individual pool */
type PoolInfo struct {
    url                 string
    api                 string
    claimed             bool
    userID              string
    height              int
    timeLastFound       time.Time
}

var globalInfo PoolsInfo

func main() {
    err := setup()

    fmt.Println("Got initial heights!")

    if err != nil {
        return
    }
    
    /* Update the height and pools in the background */
    go heightWatcher()
    go poolUpdater()
    go runApi()

    sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
    <-sc
}

func runApi() {
    http.HandleFunc("/api", helpHandler)
    http.HandleFunc("/api/", helpHandler)

    http.HandleFunc("/api/height", heightHandler)
    http.HandleFunc("/api/height/", heightHandler)

    http.HandleFunc("/api/heights", heightsHandler)
    http.HandleFunc("/api/heights/", heightsHandler)

    fmt.Println("Server started!")

    log.Fatal(http.ListenAndServe(":8080", nil))
}

func helpHandler(writer http.ResponseWriter, request *http.Request) {
    fmt.Fprintf(writer, "Supported methods:\n\n/api/height - Get median " +
                        "height\n/api/heights - Get heights of all pools")
}

func heightHandler(writer http.ResponseWriter, request *http.Request) {
    writer.Header().Set("Content-Type", "application/json")
    fmt.Fprintf(writer, "{ \"height\" : %d }", globalInfo.medianHeight)
}

func heightsHandler(writer http.ResponseWriter, request *http.Request) {
    writer.Header().Set("Content-Type", "application/json")
    
    type PoolHeight struct {
        Pool    string
        Height  int
    }

    type HeightInfo struct {
        Pools   []PoolHeight
    }

    heightInfo := HeightInfo{Pools: make([]PoolHeight, 0)}

    for _, v := range globalInfo.pools {
        pool := PoolHeight{Pool: v.url, Height: v.height}
        heightInfo.Pools = append(heightInfo.Pools, pool)
    }

    heightsJson, err := json.Marshal(heightInfo)

    if err != nil {
        fmt.Fprintf(writer, "{ \"error\" : \"Failed to convert heights to json!\" }")
        return
    }

    fmt.Fprintf(writer, string(heightsJson))
}

func setup() error {
    pools, err := getPools()

    if err != nil {
        return err
    }

    poolInfo := make([]PoolInfo, 0)

    /* Populate each pool with their info */
    for url, pool := range pools {
        var p PoolInfo
        p.url = url
        p.api = pool.Api
        poolInfo = append(poolInfo, p)
    }

    /* Update the global struct */
    globalInfo.pools = poolInfo

    populateHeights()
    updateMedianHeight()

    return nil
}

func heightWatcher() {
    for {
        time.Sleep(poolRefreshRate)
        populateHeights()
        updateMedianHeight()
    }
}

/* Update the pools json every hour */
func poolUpdater() {
    for {
        time.Sleep(time.Hour)

        pools, err := getPools()

        if err != nil {
            fmt.Println("Failed to update pools info! Error:", err)
            return
        }

        poolInfo := make([]PoolInfo, 0)

        /* Populate each pool with their info */
        for url, pool := range pools {
            var p PoolInfo
            p.url = url
            p.api = pool.Api

            /* Update it with the local pool info if it exists */
            for _, localPool := range globalInfo.pools {
                if p.url == localPool.url {
                    p.height = localPool.height
                    p.timeLastFound = localPool.timeLastFound
                    break
                }
            }

            poolInfo = append(poolInfo, p)
        }

        /* Update the global struct */
        globalInfo.pools = poolInfo
        populateHeights()
        updateMedianHeight()
    }
}

func getValues(heights map[string]int) []int {
    values := make([]int, 0)

    for _, v := range heights {
        values = append(values, v)
    }

    return values
}

func updateMedianHeight() {
    heights := make([]int, 0)

    for _, v := range globalInfo.pools {
        heights = append(heights, v.height)
    }

    median := median(heights)

    if median != globalInfo.medianHeight {
        globalInfo.medianHeight = median
        globalInfo.heightLastUpdated = time.Now()
    }
}

func median(heights []int) int {
    sort.Ints(heights)

    half := len(heights) / 2
    median := heights[half]

    if len(heights) % 2 == 0 {
        median = (median + heights[half-1]) / 2
    }

    return median
}

func populateHeights() {
    for index, _ := range globalInfo.pools {
        /* Range takes a copy of the values, we need to directly access */
        v := &globalInfo.pools[index]

        height, unix, err := getPoolHeightAndTimestamp(v.api)

        if err == nil {
            v.height = height
            v.timeLastFound = time.Unix(unix, 0)
        } else {
            v.height = 0
        }
    }
}

func getPoolHeightAndTimestamp (apiURL string) (int, int64, error) {
    statsURL := apiURL + "stats"

    http.DefaultTransport.(*http.Transport).TLSClientConfig = 
        &tls.Config{InsecureSkipVerify: true}

    resp, err := http.Get(statsURL)

    if err != nil {
        fmt.Printf("Failed to download stats from %s! Error: %s\n", 
                    statsURL, err)
        return 0, 0, err
    }

    defer resp.Body.Close()

    body, err := ioutil.ReadAll(resp.Body)

    if err != nil {
        fmt.Printf("Failed to download stats from %s! Error: %s\n",
                    statsURL, err)
        return 0, 0, err
    }

    /* Some servers (Looking at you us.turtlepool.space! send us deflate'd
       content even when we didn't ask for it - uncompress it */
    if resp.Header.Get("Content-Encoding") == "deflate" {
        body, err = ioutil.ReadAll(flate.NewReader(bytes.NewReader(body)))

        if err != nil {
            fmt.Println("Failed to deflate response from", statsURL)
            return 0, 0, err
        }
    }

    heightRegex := regexp.MustCompile(".*\"height\":(\\d+).*")
    blockFoundRegex := regexp.MustCompile(".*\"lastBlockFound\":\"(\\d+)\".*")

    height := heightRegex.FindStringSubmatch(string(body))
    blockFound := blockFoundRegex.FindStringSubmatch(string(body))

    if len(height) < 2 {
        fmt.Println("Failed to parse height from", statsURL)
        return 0, 0, errors.New("Couldn't parse height")
    }

    if len(blockFound) < 2 {
        fmt.Println("Failed to parse block last found timestamp from", statsURL)
        return 0, 0, errors.New("Couldn't parse block timestamp")
    }

    i, err := strconv.Atoi(height[1])

    if err != nil {
        fmt.Println("Failed to convert height into int! Error:", err)
        return 0, 0, err
    }

    str := blockFound[1]
    blockFound[1] = str[0:len(str) - 3]
    
    /* Don't overflow on 32 bit */
    unix, err := strconv.ParseInt(blockFound[1], 10, 64)

    if err != nil {
        fmt.Println("Failed to convert timestamp into int! Error:", err)
        return 0, 0, err
    }

    return i, unix, nil
}

func getPools() (Pools, error) {
    var pools Pools

    resp, err := http.Get(poolsJSON)

    if err != nil {
        fmt.Println("Failed to download pools json! Error:", err)
        return pools, err
    }

    defer resp.Body.Close()

    body, err := ioutil.ReadAll(resp.Body)

    if err != nil {
        fmt.Println("Failed to download pools json! Error:", err)
        return pools, err
    }

    if err := json.Unmarshal(body, &pools); err != nil {
        fmt.Println("Failed to parse pools json! Error:", err)
        return pools, err
    }

    return pools, nil
}
