package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
)

func dockerMirror(ctx context.Context, logger *log.Logger, addr string, bucket, prefix, remote string, authHost string, serverHost string, minCacheSize int64) error {
	uri, err := url.Parse(remote)
	if err != nil {
		return fmt.Errorf("parse remote: %w", err)
	}
	var router http.ServeMux
	blobCache := func(w http.ResponseWriter, r *http.Request) error {
		logger.Println("blob cache", r.URL.String())
		key := genCacheKey(prefix, r.URL.String())
		_, err = minioClient.StatObject(ctx, bucket, key, minio.GetObjectOptions{})
		if err != nil {
			resp, err := proxy(uri, r)
			if err != nil {
				return fmt.Errorf("create proxy: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNotModified {
				copyHander(w, resp)
				w.WriteHeader(resp.StatusCode)
				return nil
			}
			// 小于1M时直接返回
			if resp.ContentLength < minCacheSize {
				copyHander(w, resp)
				_, err = io.Copy(w, resp.Body)
				if err != nil {
					return fmt.Errorf("copy body: %w", err)
				}
				return nil
			}
			// 转存到s3
			_, err = minioClient.PutObject(context.Background(),
				bucket, key, resp.Body, resp.ContentLength,
				minio.PutObjectOptions{
					ContentType:    "application/octet-stream",
					SendContentMd5: true,
				},
			)
			if err != nil {
				return fmt.Errorf("put blob: %w", err)
			}
		}
		// presignedURL, err := dlMiniClient.PresignedGetObject(context.Background(),
		// 	bucket, key, time.Second*10, nil,
		// )
		// if err != nil {
		// 	fmt.Println(err)
		// 	return
		// }
		http.Redirect(w, r, dlMiniClient.EndpointURL().String()+"/"+key, http.StatusTemporaryRedirect)
		return nil
	}
	indexCache := func(w http.ResponseWriter, r *http.Request) error {
		logger.Println("index proxy", r.URL.String())
		resp, err := proxy(uri, r)
		if err != nil {
			logger.Println(err)
			return fmt.Errorf("new proxy: %w", err)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			authHeader := resp.Header.Get("Www-Authenticate")
			if authHeader != "" && authHost != "" {
				newAuthHeader := strings.ReplaceAll(authHeader, authHost, serverHost)
				resp.Header.Set("Www-Authenticate", newAuthHeader)
			}
		}
		defer resp.Body.Close()
		copyHander(w, resp)
		w.WriteHeader(resp.StatusCode)
		if resp.StatusCode == http.StatusNotModified {
			return nil
		}
		_, err = io.Copy(w, resp.Body)
		if err != nil {
			return fmt.Errorf("copy body: %w", err)
		}
		return nil
	}

	if authHost != "" {
		authUri, err := url.Parse(authHost)
		if err != nil {
			return fmt.Errorf("parse remote: %w", err)
		}
		tokenHandler := func(w http.ResponseWriter, r *http.Request) error {
			resp, err := proxy(authUri, r)
			if err != nil {
				logger.Println(err)
				return fmt.Errorf("new proxy: %w", err)
			}
			defer resp.Body.Close()
			copyHander(w, resp)
			w.WriteHeader(resp.StatusCode)
			if resp.StatusCode == http.StatusNotModified {
				return nil
			}
			_, err = io.Copy(w, resp.Body)
			if err != nil {
				return fmt.Errorf("copy body: %w", err)
			}
			return nil
		}

		router.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
			err := tokenHandler(w, r)
			if err != nil {
				logger.Println("blob cache error:", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		})
	}

	router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.String(), "/blobs/sha256:") {
			err = blobCache(w, r)
			if err != nil {
				logger.Println("blob cache error:", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		} else {
			err = indexCache(w, r)
			if err != nil {
				logger.Println("index cache error:", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		}
	})
	srv := &http.Server{
		Addr:        addr,
		BaseContext: func(net.Listener) context.Context { return ctx },
		Handler:     &router,
	}
	return srv.ListenAndServe()
}
