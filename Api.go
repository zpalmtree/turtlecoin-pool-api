package main

import (
    "strings"
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
    "time"
    "compress/flate"
    "compress/gzip"
    "bytes"
    "crypto/tls"
    "log"
)

const poolsJSON string = "https://raw.githubusercontent.com/turtlecoin/" +
                         "turtlecoin-pools-json/master/v2/turtlecoin-pools.json"

/* The amount of blocks a pool can vary from the others before we notify */
const poolMaxDifference int = 5

/* How often we check the pools */
const poolRefreshRate time.Duration = time.Second * 30

/* The data type we parse our json into */
type Pool struct {
    Url     string `json:"url"`
    Api     string `json:"api"`
    Type    string `json:"type"`
}

/* Map of pool name to pool api */
type Pools struct {
    Pools   []Pool `json:"pools"`
}

/* Info about every pool */
type PoolsInfo struct {
    pools               []PoolInfo
    modeHeight          int
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

    http.HandleFunc("/api/lastfound", lastFoundHandler)
    http.HandleFunc("/api/lastfound/", lastFoundHandler)

    http.HandleFunc("/api/forked", forkedHandler)
    http.HandleFunc("/api/forked/", forkedHandler)

    fmt.Println("Server started!")

    log.Fatal(http.ListenAndServe(":8080", nil))
}

func forkedHandler(writer http.ResponseWriter, request *http.Request) {
    writer.Header().Set("Content-Type", "application/json")

    mode := globalInfo.modeHeight

    type PoolInfo struct {
        Pool    string
        Reason  string
        Height  int
        Mode  int
    }

    type DownedInfo struct {
        Pools   []PoolInfo
    }

    downedInfo := DownedInfo{Pools: make([]PoolInfo, 0)}

    for _, v := range globalInfo.pools {
        if v.height > mode + poolMaxDifference ||
           v.height < mode - poolMaxDifference {

            reason := "forked"

            if v.height == 0 {
                reason = "api"
            }

            pool := PoolInfo{Pool: v.url, Reason: reason, Height: v.height,
                             Mode: mode}

            downedInfo.Pools = append(downedInfo.Pools, pool)
        }
    }

    downedJson, err := json.Marshal(downedInfo)

    if err != nil {
        fmt.Fprintf(writer, "{ \"error\" : \"Failed to convert to json!\" }")
        return
    }

    fmt.Fprintf(writer, string(downedJson))

}

func lastFoundHandler(writer http.ResponseWriter, request *http.Request) {
    writer.Header().Set("Content-Type", "application/json")

    timeSince := int(time.Since(globalInfo.heightLastUpdated).Minutes())

    fmt.Fprintf(writer, "{ \"mins-since-last-block\" : %d }", timeSince)
}

func helpHandler(writer http.ResponseWriter, request *http.Request) {
    fmt.Fprintf(writer, "Supported methods:\n\n" +
                        "/api - Display this help message\n" +
                        "/api/height - Get mode height\n" +
                        "/api/heights - Get heights of all pools\n" +
                        "/api/lastfound - Display the number of minutes " +
                        "since the last block was found globally\n" +
                        "/api/forked - Display any forked pools and the " +
                        "reason for the fork")
}

func heightHandler(writer http.ResponseWriter, request *http.Request) {
    writer.Header().Set("Content-Type", "application/json")
    fmt.Fprintf(writer, "{ \"height\" : %d }", globalInfo.modeHeight)
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
    for _, pool := range pools.Pools {
        var p PoolInfo

        trimmed := pool.Url

        trimmed = strings.TrimPrefix(trimmed, "https://")
        trimmed = strings.TrimPrefix(trimmed, "http://")
        trimmed = strings.TrimSuffix(trimmed, "/")

        p.url = trimmed
        p.api = pool.Api
        poolInfo = append(poolInfo, p)
    }

    /* Update the global struct */
    globalInfo.pools = poolInfo

    populateHeights()
    updatemodeHeight()

    return nil
}

func heightWatcher() {
    for {
        time.Sleep(poolRefreshRate)
        populateHeights()
        updatemodeHeight()
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
        for _, pool := range pools.Pools {
            var p PoolInfo

            trimmed := pool.Url

            trimmed = strings.TrimPrefix(trimmed, "https://")
            trimmed = strings.TrimPrefix(trimmed, "http://")
            trimmed = strings.TrimSuffix(trimmed, "/")

            p.url = trimmed
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
        updatemodeHeight()
    }
}

func getValues(heights map[string]int) []int {
    values := make([]int, 0)

    for _, v := range heights {
        values = append(values, v)
    }

    return values
}

func updatemodeHeight() {
    heights := make([]int, 0)

    for _, v := range globalInfo.pools {
        heights = append(heights, v.height)
    }

    mode := mode(heights)

    if mode != globalInfo.modeHeight {
        globalInfo.modeHeight = mode
        globalInfo.heightLastUpdated = time.Now()
    }
}

func mode(a []int) int {
    m := make(map[int]int)
    for _, v := range a {
        m[v]++
    }
    var mode []int
    var n int
    for k, v := range m {
        switch {
        case v < n:
        case v > n:
            n = v
            mode = append(mode[:0], k)
        default:
            mode = append(mode, k)
        }
    }

    return mode[0]
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

func getBody (resp *http.Response, statsURL string) ([]byte, error) {
    body, err := ioutil.ReadAll(resp.Body)

    if err != nil {
        fmt.Printf("Failed to download stats from %s! Error: %s\n",
                    statsURL, err)
        return nil, err
    }

    /* Some servers (Looking at you us.turtlepool.space!) send us deflate'd
       content even when we didn't ask for it - uncompress it */
    if resp.Header.Get("Content-Encoding") == "deflate" {
        body, err = ioutil.ReadAll(flate.NewReader(bytes.NewReader(body)))

        if err != nil {
            fmt.Println("Failed to deflate response from", statsURL)
            return nil, err
        }
    }

    /* Some pools like those under the blocks.turtle.link group appear to
       return multiple values for Content-Encoding, " ", and "gzip" */
    for k, v := range resp.Header {
        if k == "Content-Encoding" {
            for _, v1 := range v {
                if v1 == "gzip" {
                    gz, err := gzip.NewReader(bytes.NewReader(body))

                    if err != nil {
                        fmt.Println("Failed to ungzip response from %s! Error: %s\n",
                                    statsURL, err)
                        return nil, err
                    }

                    defer gz.Close()

                    body, err = ioutil.ReadAll(gz)

                    if err != nil {
                        fmt.Println("Failed to ungzip response from %s! Error: %s\n",
                                    statsURL, err)
                        return nil, err
                    }
                    break
                }
            }
        }
    }

    return body, nil
}

func parseBody(body string, statsURL string) (int, int64, error) {
    heightRegex := regexp.MustCompile(".*\"height\":(\\d+).*")
    blockFoundRegex := regexp.MustCompile(".*\"lastBlockFound\":\"(\\d+)\".*")

    height := heightRegex.FindStringSubmatch(body)
    blockFound := blockFoundRegex.FindStringSubmatch(body)

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

func getPoolHeightAndTimestamp (apiURL string) (int, int64, error) {
    statsURL := apiURL + "stats"

    http.DefaultTransport.(*http.Transport).TLSClientConfig = 
        &tls.Config{InsecureSkipVerify: true}

    timeout := time.Duration(5 * time.Second)

    client := http.Client {
        Timeout: timeout,
    }

    resp, err := client.Get(statsURL)

    if err != nil {
        fmt.Printf("Failed to download stats from %s! Error: %s\n", 
                    statsURL, err)
        return 0, 0, err
    }

    defer resp.Body.Close()

    body, err := getBody(resp, statsURL)

    if err != nil {
        return 0, 0, err
    }

    height, unix, err := parseBody(string(body), statsURL)

    if err != nil {
        return 0, 0, err
    }

    return height, unix, nil
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
