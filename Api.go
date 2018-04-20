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
    "math"
)

const poolsJSON string = "https://raw.githubusercontent.com/turtlecoin/" +
                         "turtlecoin-pools-json/master/v2/turtlecoin-pools.json"

/* The amount of blocks a pool can vary from the others before we notify */
const poolMaxDifference int = 5

/* How often we check the pools */
const poolRefreshRate time.Duration = time.Second * 15

/* The port to listen on */
const port string = ":8080"

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
    modeDifficulty      int64
}

/* Info about an individual pool */
type PoolInfo struct {
    url                 string
    api                 string
    claimed             bool
    userID              string
    height              int
    timeLastFound       time.Time
    hashrate            int64
    difficulty          int64
    poolType            string
}

var globalInfo PoolsInfo

func main() {
    err := setup()

    fmt.Println("Got initial heights!")

    if err != nil {
        return
    }
    
    /* Update the height and pools in the background */
    go statUpdater()
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

    log.Fatal(http.ListenAndServe(port, nil))
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
        Pool                string
        Height              int
        Mode                int
        LastFound           int64
        EstimatedSolveTime  int64
    }

    type HeightInfo struct {
        Pools   []PoolHeight
    }

    heightInfo := HeightInfo{Pools: make([]PoolHeight, 0)}

    for _, v := range globalInfo.pools {
        var solveTime int64

        /* Don't divide by 0! */
        if v.hashrate == 0 {
            solveTime = math.MaxInt64
        } else {
            solveTime = globalInfo.modeDifficulty / v.hashrate
        }

        var lastFound int64

        if v.timeLastFound.IsZero() {
            lastFound = math.MaxInt64
        } else {
            lastFound = int64(time.Since(v.timeLastFound).Seconds())
        }

        pool := PoolHeight{
            Pool: v.url,
            Height: v.height, 
            Mode: globalInfo.modeHeight, 
            LastFound: lastFound,
            EstimatedSolveTime: solveTime,
        }

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
        p.poolType = pool.Type

        poolInfo = append(poolInfo, p)
    }

    /* Update the global struct */
    globalInfo.pools = poolInfo

    updatePoolStats()
    updateModeHeight()
    updateModeDifficulty()

    return nil
}

func statUpdater() {
    for {
        time.Sleep(poolRefreshRate)
        updatePoolStats()
        updateModeHeight()
        updateModeDifficulty()
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
            p.poolType = pool.Type

            /* Update it with the local pool info if it exists */
            for _, localPool := range globalInfo.pools {
                if p.url == localPool.url {
                    p.height = localPool.height
                    p.timeLastFound = localPool.timeLastFound
                    p.hashrate = localPool.hashrate
                    p.difficulty = localPool.difficulty
                    break
                }
            }

            poolInfo = append(poolInfo, p)
        }

        /* Update the global struct */
        globalInfo.pools = poolInfo
        updatePoolStats()
        updateModeHeight()
    }
}

func getValues(heights map[string]int) []int {
    values := make([]int, 0)

    for _, v := range heights {
        values = append(values, v)
    }

    return values
}

func updateModeHeight() {
    heights := make([]int64, 0)

    for _, v := range globalInfo.pools {
        heights = append(heights, int64(v.height))
    }

    mode := int(mode(heights))

    if mode != globalInfo.modeHeight {
        globalInfo.modeHeight = mode
        globalInfo.heightLastUpdated = time.Now()
    }
}

func updateModeDifficulty() {
    diffs := make([]int64, 0)

    for _, v := range globalInfo.pools {
        diffs = append(diffs, v.difficulty)
    }

    globalInfo.modeDifficulty = mode(diffs)
}

func mode(a []int64) int64 {
    m := make(map[int64]int64)
    for _, v := range a {
        m[v]++
    }
    var mode []int64
    var n int64
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

func updatePoolStats() {
    for index, _ := range globalInfo.pools {
        /* Range takes a copy of the values, we need to directly access */
        v := &globalInfo.pools[index]

        height, unix, hashrate, difficulty, err := getPoolInfo(v)

        if err == nil {
            v.height = height
            v.timeLastFound = time.Unix(unix, 0)
            v.hashrate = hashrate
            v.difficulty = difficulty
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

func parseHeight(body string, statsURL string) (int, error) {
    heightRegex := regexp.MustCompile(".*\"height\":(\\d+).*")
    height := heightRegex.FindStringSubmatch(body)

    if len(height) < 2 {
        fmt.Println("Failed to parse height from", statsURL)
        return 0, errors.New("Couldn't parse height")
    }

    i, err := strconv.Atoi(height[1])

    if err != nil {
        fmt.Println("Failed to convert height into int! Error:", err)
        return 0, err
    }

    return i, nil
}

func parseForknoteBody(body string, statsURL string) (int, int64, int64, int64, error) {
    blockFoundRegex := regexp.MustCompile(".*\"lastBlockFound\":\"(\\d+)\".*")
    blockFound := blockFoundRegex.FindStringSubmatch(body)

    if len(blockFound) < 2 {
        fmt.Println("Failed to parse block last found timestamp from", statsURL)
        return 0, 0, 0, 0, errors.New("Couldn't parse block timestamp")
    }

    str := blockFound[1]
    blockFound[1] = str[0:len(str) - 3]
    
    /* Don't overflow on 32 bit */
    unix, err := strconv.ParseInt(blockFound[1], 10, 64)

    if err != nil {
        fmt.Println("Failed to convert timestamp into int! Error:", err)
        return 0, 0, 0, 0, err
    }

    i, err := parseHeight(body, statsURL)

    if err != nil {
        return 0, 0, 0, 0, err
    }

    hashrateRegex := regexp.MustCompile(".*\"hashrate\":(\\d+).*")
    hashrateStr := hashrateRegex.FindStringSubmatch(body)

    if len(hashrateStr) < 2 {
        fmt.Println("Failed to parse hashrate from", statsURL)
        return 0, 0, 0, 0, errors.New("Couldn't parse hashrate")
    }

    hashrate, err := strconv.ParseInt(hashrateStr[1], 10, 64)

    if err != nil {
        fmt.Println("Failed to convert hashrate into int! Error:", err)
        return 0, 0, 0, 0, err
    }

    difficultyRegex := regexp.MustCompile(".*\"network\":{\"difficulty\":(\\d+).*")
    difficultyStr := difficultyRegex.FindStringSubmatch(body)

    if len(difficultyStr) < 2 {
        fmt.Println("Failed to parse difficulty from", statsURL)
        return 0, 0, 0, 0, errors.New("Couldn't parse difficulty")
    }

    difficulty, err := strconv.ParseInt(difficultyStr[1], 10, 64)

    if err != nil {
        fmt.Println("Failed to convert difficulty into int! Error:", err)
        return 0, 0, 0, 0, err
    }

    return i, unix, hashrate, difficulty, nil
}

func parseForknote(p *PoolInfo) (int, int64, int64, int64, error) {
    body, err := downloadApiLink(p.api + "stats")

    if err != nil {
        return 0, 0, 0, 0, err
    }

    height, unix, hashrate, difficulty, err := parseForknoteBody(body, p.api + "stats")

    if err != nil {
        return 0, 0, 0, 0, err
    }

    return height, unix, hashrate, difficulty, nil
}

func downloadApiLink(apiURL string) (string, error) {
    http.DefaultTransport.(*http.Transport).TLSClientConfig = 
        &tls.Config{InsecureSkipVerify: true}

    timeout := time.Duration(5 * time.Second)

    client := http.Client {
        Timeout: timeout,
    }

    resp, err := client.Get(apiURL)

    if err != nil {
        fmt.Printf("Failed to download stats from %s! Error: %s\n", 
                    apiURL, err)
        return "", err
    }

    defer resp.Body.Close()

    body, err := getBody(resp, apiURL)

    if err != nil {
        return "", err
    }

    return string(body), nil
}

func parseNodeJS(p *PoolInfo) (int, int64, int64, int64, error) {
    networkURL := p.api + "network/stats"
    poolURL := p.api + "pool/stats"

    networkBody, err := downloadApiLink(networkURL)

    if err != nil {
        return 0, 0, 0, 0, err
    }

    poolBody, err := downloadApiLink(poolURL)

    if err != nil {
        return 0, 0, 0, 0, err
    }

    blockFoundRegex := regexp.MustCompile(".*\"lastBlockFoundTime\":(\\d+).*")
    blockFound := blockFoundRegex.FindStringSubmatch(poolBody)

    if len(blockFound) < 2 {
        fmt.Println("Failed to parse block last found timestamp from", poolURL)
        return 0, 0, 0, 0, errors.New("Couldn't parse block timestamp")
    }

    /* Don't overflow on 32 bit */
    unix, err := strconv.ParseInt(blockFound[1], 10, 64)

    if err != nil {
        fmt.Println("Failed to convert timestamp into int! Error:", err)
        return 0, 0, 0, 0, err
    }

    i, err := parseHeight(networkBody, networkURL)

    if err != nil {
        return 0, 0, 0, 0, err
    }

    hashrateRegex := regexp.MustCompile(".*\"hashRate\":(\\d+).*")
    hashrateStr := hashrateRegex.FindStringSubmatch(poolBody)

    if len(hashrateStr) < 2 {
        fmt.Println("Failed to parse hashrate from", poolURL)
        return 0, 0, 0, 0, errors.New("Couldn't parse hashrate")
    }

    hashrate, err := strconv.ParseInt(hashrateStr[1], 10, 64)

    if err != nil {
        fmt.Println("Failed to convert hashrate into int! Error:", err)
        return 0, 0, 0, 0, err
    }

    difficultyRegex := regexp.MustCompile(".*\"difficulty\":(\\d+).*")
    difficultyStr := difficultyRegex.FindStringSubmatch(networkBody)

    if len(difficultyStr) < 2 {
        fmt.Println("Failed to parse difficulty from", networkURL)
        return 0, 0, 0, 0, errors.New("Couldn't parse difficulty")
    }

    difficulty, err := strconv.ParseInt(difficultyStr[1], 10, 64)

    if err != nil {
        fmt.Println("Failed to convert difficulty into int! Error:", err)
        return 0, 0, 0, 0, err
    }

    return i, unix, hashrate, difficulty, nil
}

func getPoolInfo (p *PoolInfo) (int, int64, int64, int64, error) {
    var height int
    var unix int64
    var hashrate int64
    var difficulty int64
    var err error

    if p.poolType == "forknote" {
        height, unix, hashrate, difficulty, err = parseForknote(p)
    } else if p.poolType == "node.js" {
        height, unix, hashrate, difficulty, err = parseNodeJS(p)
    } else {
        fmt.Println("Unknown pool type", p.poolType, "skipping.")
        return 0, 0, 0, 0, errors.New("Unknown pool type")
    }

    if err != nil {
        return 0, 0, 0, 0, err
    }

    return height, unix, hashrate, difficulty, nil
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
