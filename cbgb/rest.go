package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

func adminRequired(req *http.Request, rm *mux.RouteMatch) bool {
	log.Printf("Verifying admin at %v -> %v", req.URL, rm)
	return true
}

func mkAuthHandler(r *mux.Router, path string,
	f func(http.ResponseWriter, *http.Request)) *mux.Route {
	return r.HandleFunc(path, f).MatcherFunc(adminRequired)
}

func mkAdminHandler(r *mux.Router, path string,
	f func(http.ResponseWriter, *http.Request)) *mux.Route {
	return r.HandleFunc(path, f).MatcherFunc(adminRequired)
}

func restAPI(r *mux.Router, staticPath string) {
	mkAdminHandler(r, "/_api/buckets",
		restGetBuckets).Methods("GET")
	mkAdminHandler(r, "/_api/buckets",
		restPostBucket).Methods("POST")
	mkAdminHandler(r, "/_api/buckets/{bucketName}",
		restGetBucket).Methods("GET")
	mkAdminHandler(r, "/_api/buckets/{bucketName}",
		restDeleteBucket).Methods("DELETE")
	mkAdminHandler(r, "/_api/buckets/{bucketName}/compact",
		restPostBucketCompact).Methods("POST")
	mkAdminHandler(r, "/_api/buckets/{bucketName}/flushDirty",
		restPostBucketFlushDirty).Methods("POST")
	mkAdminHandler(r, "/_api/buckets/{bucketName}/stats",
		restGetBucketStats).Methods("GET")
	mkAdminHandler(r, "/_api/bucketsRescan",
		restPostBucketsRescan).Methods("POST")
	mkAdminHandler(r, "/_api/profile/cpu",
		restProfileCPU).Methods("POST")
	mkAdminHandler(r, "/_api/profile/memory",
		restProfileMemory).Methods("POST")
	mkAdminHandler(r, "/_api/runtime",
		restGetRuntime).Methods("GET")
	mkAdminHandler(r, "/_api/runtime/memStats",
		restGetRuntimeMemStats).Methods("GET")
	mkAdminHandler(r, "/_api/runtime/gc",
		restPostRuntimeGC).Methods("POST")
	mkAdminHandler(r, "/_api/settings",
		restGetSettings).Methods("GET")

	initStatic(r, "/_static/", staticPath)
}

func initStatic(r *mux.Router, staticPrefix, staticPath string) {
	if strings.HasPrefix(staticPath, "http://") {
		zs, err := zipStatic(staticPath)
		if err != nil {
			log.Fatalf("Error initializing zip static: %v", err)
		}
		r.PathPrefix(staticPrefix).Handler(
			http.StripPrefix(staticPrefix, zs))
	} else {
		r.PathPrefix(staticPrefix).Handler(
			http.StripPrefix(staticPrefix,
				http.FileServer(http.Dir(staticPath))))
	}
}

// For settings that are constant throughout server process lifetime.
func restGetSettings(w http.ResponseWriter, r *http.Request) {
	jsonEncode(w, map[string]interface{}{
		"addr":              *addr,
		"data":              *data,
		"rest-couch":        *restCouch,
		"rest-ns":           *restNS,
		"static-path":       *staticPath,
		"defaultBucketName": *defaultBucketName,
		"bucketSettings":    bucketSettings,
		"verbose":           *verbose,
	})
}

func restGetBuckets(w http.ResponseWriter, r *http.Request) {
	jsonEncode(w, buckets.GetNames())
}

func restPostBucketsRescan(w http.ResponseWriter, r *http.Request) {
	err := buckets.Load(true)
	if err != nil {
		http.Error(w,
			fmt.Sprintf("rescanning/reloading buckets directory err: %v", err), 500)
		return
	}
	http.Redirect(w, r, "/_api/buckets", 303)
}

func restPostBucket(w http.ResponseWriter, r *http.Request) {
	bucketName := r.FormValue("name")
	if len(bucketName) < 1 {
		http.Error(w, "bucket name is too short or is missing", 400)
		return
	}
	match, err := regexp.MatchString("^[A-Za-z0-9\\-_]+$", bucketName)
	if err != nil || !match {
		http.Error(w,
			fmt.Sprintf("illegal bucket name: %v, err: %v", bucketName, err), 400)
		return
	}

	bSettings := bucketSettings.Copy()
	bucketPassword := r.FormValue("password")
	if bucketPassword != "" {
		bSettings.PasswordHash = bucketPassword
	}
	bSettings.QuotaBytes = getIntValue(r, "quotaBytes",
		bucketSettings.QuotaBytes)
	bSettings.MemoryOnly = int(getIntValue(r, "memoryOnly",
		int64(bucketSettings.MemoryOnly)))

	_, err = createBucket(bucketName, bSettings)
	if err != nil {
		http.Error(w,
			fmt.Sprintf("create bucket error; name: %v, err: %v", bucketName, err), 500)
		return
	}
	http.Redirect(w, r, "/_api/buckets/"+bucketName, 303)
}

func restGetBucket(w http.ResponseWriter, r *http.Request) {
	bucketName, bucket := parseBucketName(w, r)
	if bucket == nil {
		return
	}
	partitions := map[string]interface{}{}
	settings := bucket.GetBucketSettings()
	for vbid := uint16(0); vbid < uint16(settings.NumPartitions); vbid++ {
		vb, _ := bucket.GetVBucket(vbid)
		if vb != nil {
			vbm := vb.Meta()
			if vbm != nil {
				partitions[strconv.Itoa(int(vbm.Id))] = vbm
			}
		}
	}
	vb := bucket.GetDDocVBucket()
	if vb != nil {
		vbm := vb.Meta()
		if vbm != nil {
			partitions[strconv.Itoa(int(vbm.Id))] = vbm
		}
	}
	jsonEncode(w, map[string]interface{}{
		"name":       bucketName,
		"itemBytes":  bucket.GetItemBytes(),
		"settings":   settings.SafeView(),
		"partitions": partitions,
	})
}

func restDeleteBucket(w http.ResponseWriter, r *http.Request) {
	bucketName, bucket := parseBucketName(w, r)
	if bucket == nil {
		return
	}
	buckets.Close(bucketName, true)
}

func restPostBucketCompact(w http.ResponseWriter, r *http.Request) {
	bucketName, bucket := parseBucketName(w, r)
	if bucket == nil {
		return
	}
	if err := bucket.Compact(); err != nil {
		http.Error(w, fmt.Sprintf("error compacting bucket: %v, err: %v",
			bucketName, err), 500)
	}
}

func restPostBucketFlushDirty(w http.ResponseWriter, r *http.Request) {
	bucketName, bucket := parseBucketName(w, r)
	if bucket == nil {
		return
	}
	if err := bucket.Flush(); err != nil {
		http.Error(w, fmt.Sprintf("error flushing bucket: %v, err: %v",
			bucketName, err), 500)
	}
}

func restGetBucketStats(w http.ResponseWriter, r *http.Request) {
	_, bucket := parseBucketName(w, r)
	if bucket == nil {
		return
	}
	st := bucket.SnapshotStats()
	if time.Since(st.LatestUpdateTime()) > time.Second*30 {
		bucket.StartStats(time.Second)
		// Go ahead and let this delay slightly to catch up
		// the stats.
		time.Sleep(time.Millisecond * 2100)
		st = bucket.SnapshotStats()
	}
	jsonEncode(w, st.ToMap())
}

// To start a cpu profiling...
//    curl -X POST http://127.0.0.1:8077/_api/profile/cpu -d secs=5
// To analyze a profiling...
//    go tool pprof ./cbgb/cbgb run-cpu.pprof
func restProfileCPU(w http.ResponseWriter, r *http.Request) {
	fname := "./run-cpu.pprof"
	secs, err := strconv.Atoi(r.FormValue("secs"))
	if err != nil || secs <= 0 {
		http.Error(w, "incorrect or missing secs parameter", 400)
		return
	}
	os.Remove(fname)
	f, err := os.Create(fname)
	if err != nil {
		http.Error(w, fmt.Sprintf("couldn't create file: %v, err: %v",
			fname, err), 500)
		return
	}

	pprof.StartCPUProfile(f)
	go func() {
		<-time.After(time.Duration(secs) * time.Second)
		pprof.StopCPUProfile()
		f.Close()
	}()
}

// To grab a memory profiling...
//    curl -X POST http://127.0.0.1:8077/_api/profile/memory
// To analyze a profiling...
//    go tool pprof ./cbgb/cbgb run-memory.pprof
func restProfileMemory(w http.ResponseWriter, r *http.Request) {
	fname := "./run-memory.pprof"
	os.Remove(fname)
	f, err := os.Create(fname)
	if err != nil {
		http.Error(w, fmt.Sprintf("couldn't create file: %v, err: %v",
			fname, err), 500)
		return
	}
	defer f.Close()
	pprof.WriteHeapProfile(f)
}

func restGetRuntime(w http.ResponseWriter, r *http.Request) {
	jsonEncode(w, map[string]interface{}{
		"startTime": startTime,
		"arch":      runtime.GOARCH,
		"os":        runtime.GOOS,
		"numCPU":    runtime.NumCPU(),
		"go": map[string]interface{}{
			"GOMAXPROCS":     runtime.GOMAXPROCS(0),
			"GOROOT":         runtime.GOROOT(),
			"version":        runtime.Version(),
			"numGoroutine":   runtime.NumGoroutine(),
			"numCgoCall":     runtime.NumCgoCall(),
			"compiler":       runtime.Compiler,
			"memProfileRate": runtime.MemProfileRate,
		},
	})
}

func restGetRuntimeMemStats(w http.ResponseWriter, r *http.Request) {
	memStats := &runtime.MemStats{}
	runtime.ReadMemStats(memStats)
	jsonEncode(w, memStats)
}

func restPostRuntimeGC(w http.ResponseWriter, r *http.Request) {
	runtime.GC()
}
