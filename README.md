# turtlecoin-pool-api

## Prerequisites

* A semi recent version of go. The one provided by the default ubuntu repositories might be too old. You can use the binary distributions from the golang website successfully on ubuntu.

## Building

`go build Api.go`

## Running

`./Api`

## Endpoints

Open up your webbrowser and navigate to localhost:8080/api

* /api - Lists how to use the api
* /api/height - Lists the median height of all pools
* /api/heights - Lists the heights of all known pools
* /api/lastfound - Prints the time in minutes since the last block was found globally
* /api/forked - Prints pools that are behind/ahead/downed api, their height, and the reason
