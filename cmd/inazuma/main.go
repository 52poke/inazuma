package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/52poke/inazuma/internal/cache"
	"github.com/52poke/inazuma/internal/config"
	"github.com/52poke/inazuma/internal/http"
	"github.com/52poke/inazuma/internal/lock"
	"github.com/52poke/inazuma/internal/mw"
	"github.com/52poke/inazuma/internal/purge"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const methodPurge = "PURGE"

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(cfg.S3Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, "")),
	)
	if err != nil {
		log.Fatal(err)
	}

	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.BaseEndpoint = aws.String(cfg.S3Endpoint)
	})
	store := cache.NewS3Store(cfg.S3Bucket, s3Client)
	redisClient := lock.NewRedisClient(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	mwClient := mw.NewClient(cfg.MediaWikiBaseURL)

	handler, err := httpx.NewHandler(cfg, store, mwClient, redisClient)
	if err != nil {
		log.Fatal(err)
	}

	purgeHandler := &purge.Handler{
		Cache:      store,
		MW:         mwClient,
		Redis:      redisClient,
		NginxPurge: cfg.NginxPurgeURL,
		LockTTL:    time.Duration(cfg.LockTTLSeconds) * time.Second,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == methodPurge {
			purgeHandler.ServeHTTP(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	}))

	server := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("listening on %s", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
