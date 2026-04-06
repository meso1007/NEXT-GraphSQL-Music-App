// backend/main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

func main() {
	// 1. 環境変数の読み込み
	if err := godotenv.Load(); err != nil {
		log.Println("⚠️ .env file not found")
	}

	// 2. Last.fm API Keyの取得
	apiKey := os.Getenv("LASTFM_API_KEY")

	// 3. Neo4j 接続設定
	ctx := context.Background()
	dbUri := os.Getenv("NEO4J_URI")
	dbUser := os.Getenv("NEO4J_USER")
	dbPassword := os.Getenv("NEO4J_PASSWORD")

	driver, err := neo4j.NewDriverWithContext(dbUri, neo4j.BasicAuth(dbUser, dbPassword, ""))
	if err != nil {
		log.Fatal(err)
	}
	defer driver.Close(ctx)

	// 4. 【重要】ここでSeed実行 (引数を渡す)
	if apiKey != "" {
		seedMusicData(ctx, driver, apiKey)
	} else {
		log.Println("⚠️ LASTFM_API_KEY が .env に設定されていないため Seed 処理をスキップしました")
	}

	// 5. API サーバー設定
	r := gin.Default()

	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Origin, Content-Type")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	r.GET("/api/graph", func(c *gin.Context) {
		session := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
		defer session.Close(ctx)

		// 1. ノード(Artist)の取得
		nodesRes, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
			result, err := tx.Run(ctx, "MATCH (n:Artist) RETURN n.name AS id LIMIT 200", nil)
			if err != nil {
				return nil, err
			}
			var nodes []map[string]interface{}
			for result.Next(ctx) {
				id, _ := result.Record().Get("id")
				nodes = append(nodes, map[string]interface{}{"id": id, "group": "Artist"})
			}
			return nodes, nil
		})
		if err != nil {
			log.Println("Node query error:", err)
			c.JSON(500, gin.H{"error": "Failed to fetch nodes"})
			return
		}

		// 2. リンク(SIMILAR_TO)の取得
		linksRes, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
			result, err := tx.Run(ctx, "MATCH (n:Artist)-[:SIMILAR_TO]->(m:Artist) RETURN n.name AS source, m.name AS target LIMIT 500", nil)
			if err != nil {
				return nil, err
			}
			var links []map[string]interface{}
			for result.Next(ctx) {
				source, _ := result.Record().Get("source")
				target, _ := result.Record().Get("target")
				links = append(links, map[string]interface{}{"source": source, "target": target})
			}
			return links, nil
		})
		if err != nil {
			log.Println("Link query error:", err)
			c.JSON(500, gin.H{"error": "Failed to fetch links"})
			return
		}

		c.JSON(200, gin.H{
			"nodes": nodesRes,
			"links": linksRes,
		})
	})

	r.Run(":8080")
}

// アーティストをDBに保存（または更新）する関数
func saveArtistToNeo4j(ctx context.Context, driver neo4j.DriverWithContext, name string) error {
	session := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)

	// 今回はSpotify IDが使えないので、アーティスト名を一意のキーとして扱います
	query := `
		MERGE (a:Artist {name: $name})
		RETURN a
	`
	params := map[string]interface{}{
		"name": name,
	}

	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		return tx.Run(ctx, query, params)
	})
	return err
}

// 二人のアーティストの間に「似ている」という線を引く関数
func connectArtists(ctx context.Context, driver neo4j.DriverWithContext, name1, name2 string) error {
	session := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)

	// 名前に基づいてリレーションを作成
	query := `
		MATCH (a:Artist {name: $name1})
		MATCH (b:Artist {name: $name2})
		MERGE (a)-[:SIMILAR_TO]->(b)
	`
	params := map[string]interface{}{"name1": name1, "name2": name2}

	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		return tx.Run(ctx, query, params)
	})
	return err
}

type LastFMArtist struct {
	Name string `json:"name"`
	Mbid string `json:"mbid"` // 使わないかもしれませんが持っておきます
}

type LastFMSimilarArtists struct {
	Artist []LastFMArtist `json:"artist"`
}

type LastFMResponse struct {
	SimilarArtists LastFMSimilarArtists `json:"similarartists"`
}

func getSimilarArtistsLastFM(apiKey string, artistName string) ([]LastFMArtist, error) {
	// Last.fm APIで類似アーティストを取得 (20件に制限)
	apiUrl := fmt.Sprintf("http://ws.audioscrobbler.com/2.0/?method=artist.getsimilar&artist=%s&api_key=%s&format=json&limit=20",
		url.QueryEscape(artistName), apiKey)

	resp, err := http.Get(apiUrl)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Last.fm API error: status %d - Body: %s", resp.StatusCode, string(bodyBytes))
	}

	var result LastFMResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.SimilarArtists.Artist, nil
}

func seedMusicData(ctx context.Context, driver neo4j.DriverWithContext, apiKey string) {
	startName := "Nas"

	log.Printf("🌱 Seeding started for: %s", startName)

	// 1. 起点のNasを保存
	if err := saveArtistToNeo4j(ctx, driver, startName); err != nil {
		log.Println("❌ Failed to save start artist:", err)
		return
	}

	// 2. Nasに関連するアーティストを取得
	related, err := getSimilarArtistsLastFM(apiKey, startName)
	if err != nil {
		log.Println("❌ Last.fm API Error:", err)
		return
	}

	log.Printf("🔍 Found %d similar artists for %s", len(related), startName)

	// 3. 取得したアーティストをDBに保存してNasと繋ぐ
	for i, artist := range related {
		log.Printf("[%d/%d] Saving & Connecting: %s", i+1, len(related), artist.Name)

		if err := saveArtistToNeo4j(ctx, driver, artist.Name); err != nil {
			log.Printf("  ⚠️ Failed to save %s: %v", artist.Name, err)
			continue
		}

		if err := connectArtists(ctx, driver, startName, artist.Name); err != nil {
			log.Printf("  ⚠️ Failed to connect %s -> %s: %v", startName, artist.Name, err)
		}
	}
	log.Println("🎨 Music Graph Data Seeded Successfully!")
}
