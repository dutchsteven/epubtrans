package cmd

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"embed"

	"github.com/PuerkitoBio/goquery"
	"github.com/gofiber/fiber/v2"
	"github.com/liushuangls/go-anthropic/v2"
	"github.com/dutchsteven/epubtrans/pkg/loader"
	"github.com/dutchsteven/epubtrans/pkg/translator"
	"github.com/dutchsteven/epubtrans/pkg/util"
	"github.com/spf13/cobra"
)

//go:embed assets/app.js assets/app.css assets/index.html
var embeddedAssets embed.FS

var Serve = &cobra.Command{
	Use:   "serve [unpackedEpubPath]",
	Short: "Serve the content of an unpacked EPUB as a web server",
	Long:  `This command starts a web server that serves the content of an unpacked EPUB file. You can access the EPUB content through your web browser. Make sure to provide the path to the unpacked EPUB directory.`,
	Example: `epubtrans serve path/to/unpacked/epub
		# This will start the server and serve the EPUB content at http://localhost:3000`,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("unpackedEpubPath is required. Please provide the path to the unpacked EPUB directory.")
		}

		return util.ValidateEpubPath(args[0])
	},
	RunE: runServe,
}

func init() {
	// port flag
	Serve.Flags().StringP("port", "p", "3000", "port to serve the EPUB content")
}

var ToInjectContentTypes = []string{
	"text/html",
	"application/xhtml+xml",
	"application/xml",
	"text/plain",
}

func shouldInject(contentType string) bool {
	for _, ct := range ToInjectContentTypes {
		if strings.Contains(contentType, ct) {
			return true
		}
	}
	return false
}

type TranslateRequest struct {
	FilePath           string `json:"file_path"`
	TranslationID      string `json:"translation_id"`
	TranslationContent string `json:"translation_content"`
}

type NavPoint struct {
	XMLName   xml.Name   `xml:"navPoint"`
	ID        string     `xml:"id,attr"`
	PlayOrder string     `xml:"playOrder,attr"`
	NavLabel  NavLabel   `xml:"navLabel"`
	Content   Content    `xml:"content"`
	NavPoints []NavPoint `xml:"navPoint"`
}

type NavLabel struct {
	Text string `xml:"text"`
}

type Content struct {
	Src string `xml:"src,attr"`
}

type NCX struct {
	XMLName xml.Name `xml:"ncx"`
	NavMap  NavMap   `xml:"navMap"`
}

type NavMap struct {
	NavPoints []NavPoint `xml:"navPoint"`
}

func generateTOCHTML(navPoints []NavPoint, level int) string {
	if len(navPoints) == 0 {
		return ""
	}

	var html strings.Builder
	html.WriteString("<ul>")

	for _, np := range navPoints {
		html.WriteString(fmt.Sprintf("<li><a target=\"_blank\" href=\"%s\">%s</a>", np.Content.Src, np.NavLabel.Text))
		if len(np.NavPoints) > 0 {
			html.WriteString(generateTOCHTML(np.NavPoints, level+1))
		}
		html.WriteString("</li>")
	}

	html.WriteString("</ul>")
	return html.String()
}

const (
	githubRawContent = "https://raw.githubusercontent.com"
	userRepo         = "dutchsteven/epubtrans"
	branch           = "main"
)

type TranslateAIRequest struct {
    FilePath      string `json:"file_path"`
    TranslationID string `json:"translation_id"`
    ContentID     string `json:"content_id"`
    Instructions  string `json:"instructions"`
}

// Add this function to call the AI translation service (you'll need to implement this)
func translateWithAI(content string, instructions string, bookTitle string) (string, error) {
    ctx := context.Background()

    // Create an Anthropic translator
    anthropicTranslator, err := translator.GetAnthropicTranslator(&translator.Config{
        APIKey:      os.Getenv("ANTHROPIC_KEY"),
        Model:       string(anthropic.ModelClaude3Dot5SonnetLatest), // You might want to make this configurable
        Temperature: 0.7,
        MaxTokens:   8192,
    })
    if err != nil {
        return "", fmt.Errorf("error getting translator: %v", err)
    }

    // Translate the content
    translatedContent, err := anthropicTranslator.Translate(ctx, instructions, content, "english", "vietnamese", bookTitle)
    if err != nil {
        return "", fmt.Errorf("translation error: %v", err)
    }

    return translatedContent, nil
}

func runServe(cmd *cobra.Command, args []string) error {
	unpackedEpubPath := args[0]

	// Check if the directory exists
	if _, err := os.Stat(unpackedEpubPath); os.IsNotExist(err) {
		return fmt.Errorf("the specified directory does not exist: %s", unpackedEpubPath)
	}

	// Try to parse the container - make it optional for upload-only mode
	container, err := loader.ParseContainer(unpackedEpubPath)
	var pkg *loader.Package
	var bookTitle string
	
	if err != nil {
		slog.Warn("No EPUB package found, running in upload-only mode: " + err.Error())
		// Create a dummy package for upload-only mode
		pkg = &loader.Package{
			Metadata: loader.Metadata{
				Title: "Untitled",
			},
			Manifest: loader.Manifest{
				Items: []loader.Item{},
			},
			Spine: loader.Spine{
				ItemRefs: []loader.ItemRef{},
			},
		}
		bookTitle = "Untitled"
	} else {
		opfPath := filepath.Join(unpackedEpubPath, container.Rootfile.FullPath)
		pkg, err = loader.ParsePackage(opfPath)
		if err != nil {
			return fmt.Errorf("error parsing package: %v", err)
		}
		bookTitle = pkg.Metadata.Title
	}

	slog.Info("Book title: " + bookTitle)

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
	})

	// Serve the main index.html
	app.Get("/", func(c *fiber.Ctx) error {
		index, err := embeddedAssets.ReadFile("assets/index.html")
		if err != nil {
			return c.Status(500).SendString("Error loading index.html")
		}
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.Send(index)
	})

	// Serve assets
	app.Get("/assets/app.js", func(c *fiber.Ctx) error {
		js, err := embeddedAssets.ReadFile("assets/app.js")
		if err != nil {
			return c.Status(500).SendString("Error loading app.js")
		}
		c.Set("Content-Type", "application/javascript; charset=utf-8")
		return c.Send(js)
	})

	app.Get("/assets/app.css", func(c *fiber.Ctx) error {
		css, err := embeddedAssets.ReadFile("assets/app.css")
		if err != nil {
			return c.Status(500).SendString("Error loading app.css")
		}
		c.Set("Content-Type", "text/css; charset=utf-8")
		return c.Send(css)
	})

	// Proxy route for assets (fallback to GitHub)
	app.Get("/assets/:filename", func(c *fiber.Ctx) error {

		filename := c.Params("filename")
		if filename == "app.js" || filename == "app.css" {
			content, err := embeddedAssets.ReadFile("assets/" + filename)
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).SendString("Error fetching file")
			}
			return c.Send(content)
		}

		url := fmt.Sprintf("%s/%s/%s/assets/%s", githubRawContent, userRepo, branch, filename)

		// Make request to GitHub
		resp, err := http.Get(url)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString("Error fetching file")
		}
		defer resp.Body.Close()

		// Set content type based on file extension
		if strings.HasSuffix(filename, ".css") {
			c.Set("Content-Type", "text/css")
		} else if strings.HasSuffix(filename, ".js") {
			c.Set("Content-Type", "application/javascript")
		}

		//send the body to the client
		body, _ := io.ReadAll(resp.Body)
		return c.Send(body)
	})

	// Set content directory path - use unpackedEpubPath if container is nil
	contentDirPath := unpackedEpubPath
	if container != nil {
		contentDirPath = path.Dir(path.Join(unpackedEpubPath, container.Rootfile.FullPath))
	}

	app.Get("/toc.html", func(c *fiber.Ctx) error {
		opfPath := filepath.Join(unpackedEpubPath, container.Rootfile.FullPath)
		pkg, err := loader.ParsePackage(opfPath)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString(fmt.Sprintf("Error parsing package: %v", err))
		}

		tocItem := pkg.Manifest.GetItemByID(pkg.Spine.Toc)

		if tocItem == nil {
			return c.Status(fiber.StatusInternalServerError).SendString("Error getting toc item")
		}

		tocPath := path.Join(contentDirPath, tocItem.Href)
		// Read the toc.ncx file
		tocContent, err := ioutil.ReadFile(tocPath)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString("Error reading toc.ncx file: " + tocPath)
		}

		var ncx NCX
		err = xml.Unmarshal(tocContent, &ncx)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString("Error parsing toc.ncx file")
		}

		// Generate HTML TOC
		tocHTML := generateTOCHTML(ncx.NavMap.NavPoints, 0)

		// Wrap the TOC in a basic HTML structure
		fullHTML := fmt.Sprintf(`
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Table of Contents</title>
    <style>
        body { font-family: Arial, sans-serif; line-height: 1.6; }
        ul { padding-left: 20px; }
    </style>
</head>
<body>
    <h1>Table of Contents</h1>
    %s
</body>
</html>
`, tocHTML)

		c.Set("Content-Type", "text/html")

		return c.SendString(fullHTML)
	})

	app.Static("/", contentDirPath, fiber.Static{
		Browse: true,
		ModifyResponse: func(c *fiber.Ctx) error {
			contentType := c.Response().Header.Peek("Content-Type")
			if !shouldInject(string(contentType)) {
				return nil
			}

			body := c.Response().Body()
			if len(body) == 0 {
				return nil
			}

			// Find the position of </body>
			pos := bytes.LastIndex(body, []byte("</body>"))
			if pos == -1 {
				return nil
			}

			// Inject the script before </body>
			newBody := append(append(body[:pos], []byte(`<script src="/assets/app.js"></script><link rel="stylesheet" href="/assets/app.css">`)...), body[pos:]...)

			c.Response().SetBody(newBody)
			c.Response().Header.SetContentLength(len(newBody))
			return nil
		},
	})

	// Upload and translate endpoint
	app.Post("/api/translate", func(c *fiber.Ctx) error {
		file, err := c.FormFile("epub")
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "No EPUB file provided"})
		}

		sourceLang := c.FormValue("source", "English")
		targetLang := c.FormValue("target", "Dutch")
		isBilingual := c.FormValue("bilingual") == "true"

		// Save uploaded file
		uploadPath := filepath.Join("/app/uploads", file.Filename)
		if err := c.SaveFile(file, uploadPath); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to save file: " + err.Error()})
		}

		// Get the actual unpack destination
		unpackDir, err := util.GetUnzipDestination(uploadPath)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to determine unpack path: " + err.Error()})
		}

		// Run unpack command
		if err := Unpack.RunE(&cobra.Command{}, []string{uploadPath}); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to unpack EPUB: " + err.Error()})
		}

		// Clean
		cleanCmd := &cobra.Command{}
		cleanCmd.SetContext(context.Background())
		cleanCmd.Flags().Int("workers", runtime.NumCPU(), "Number of worker goroutines")
		if err := Clean.RunE(cleanCmd, []string{unpackDir}); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to clean EPUB: " + err.Error()})
		}

		// Mark
		markCmd := &cobra.Command{}
		markCmd.SetContext(context.Background())
		markCmd.Flags().Int("workers", runtime.NumCPU(), "Number of worker goroutines")
		if err := Mark.RunE(markCmd, []string{unpackDir}); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to mark EPUB: " + err.Error()})
		}

		// Translate
		os.Setenv("SOURCE_LANGUAGE", sourceLang)
		os.Setenv("TARGET_LANGUAGE", targetLang)
		translateCmd := &cobra.Command{}
		translateCmd.SetContext(context.Background())
		if err := Translate.RunE(translateCmd, []string{unpackDir}); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to translate: " + err.Error()})
		}

		// Styling
		stylingCmd := &cobra.Command{}
		stylingCmd.SetContext(context.Background())
		stylingCmd.Flags().String("hide", "", "Hide source or target text")
		if isBilingual {
			if err := Styling.RunE(stylingCmd, []string{unpackDir}); err != nil {
				slog.Warn("Styling failed: " + err.Error())
			}
		} else {
			stylingCmd.Flags().Set("hide", "source")
			if err := Styling.RunE(stylingCmd, []string{unpackDir}); err != nil {
				slog.Warn("Styling failed: " + err.Error())
			}
		}

		// Pack
		packDir := filepath.Join("/app/uploads", strings.TrimSuffix(file.Filename, ".epub")+"-translated.epub")
		packCmd := &cobra.Command{}
		packCmd.SetContext(context.Background())
		if err := Pack.RunE(packCmd, []string{unpackDir, packDir}); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to pack translated book: " + err.Error()})
		}

		return c.JSON(fiber.Map{
			"message":      "Translation completed successfully",
			"output_dir":   packDir,
			"source_lang":  sourceLang,
			"target_lang":  targetLang,
			"bilingual":    isBilingual,
		})
	})

	app.Post("/api/update-translation", func(c *fiber.Ctx) error {
		var req TranslateRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "Invalid request"})
		}

		filePath := path.Join(contentDirPath, req.FilePath)
		// Read the file
		content, err := os.ReadFile(filePath)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to read file"})
		}

		// Parse the HTML
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(content)))
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to parse HTML"})
		}

		// Find the element and update its content
		updated := false
		doc.Find("[data-translation-id]").Each(func(i int, s *goquery.Selection) {
			if id, exists := s.Attr("data-translation-id"); exists && id == req.TranslationID {
				s.SetHtml(req.TranslationContent)
				updated = true
			}
		})

		if !updated {
			return c.Status(404).JSON(fiber.Map{"error": "Translation ID not found"})
		}

		// Write the updated content back to the file
		html, err := doc.Html()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to generate HTML"})
		}

		err = os.WriteFile(filePath, []byte(html), 0644)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Failed to write file"})
		}

		return c.JSON(fiber.Map{"message": "Translation updated successfully"})
	})

	// API endpoint to get ebook information
	app.Get("/api/info", func(c *fiber.Ctx) error {
		opfPath := filepath.Join(unpackedEpubPath, container.Rootfile.FullPath)
		pkg, err := loader.ParsePackage(opfPath)
		if err != nil {
			return c.Status(500).SendString(fmt.Sprintf("Error parsing package: %v", err))
		}

		return c.JSON(pkg.Metadata)
	})

	// API endpoint to get manifest items
	app.Get("/api/manifest", func(c *fiber.Ctx) error {
		opfPath := filepath.Join(unpackedEpubPath, container.Rootfile.FullPath)
		pkg, err := loader.ParsePackage(opfPath)
		if err != nil {
			return c.Status(500).SendString(fmt.Sprintf("Error parsing package: %v", err))
		}

		return c.JSON(pkg.Manifest)
	})

	// API endpoint to get spine items
	app.Get("/api/spine", func(c *fiber.Ctx) error {
		opfPath := filepath.Join(unpackedEpubPath, container.Rootfile.FullPath)
		pkg, err := loader.ParsePackage(opfPath)
		if err != nil {
			return c.Status(500).SendString(fmt.Sprintf("Error parsing package: %v", err))
		}

		return c.JSON(pkg.Spine)
	})

	app.Post("/api/ai-translate", func(c *fiber.Ctx) error {
        var req TranslateAIRequest
        if err := c.BodyParser(&req); err != nil {
            return c.Status(400).JSON(fiber.Map{"error": "Invalid request"})
        }

        filePath := path.Join(contentDirPath, req.FilePath)
        content, err := os.ReadFile(filePath)
        if err != nil {
            return c.Status(500).JSON(fiber.Map{"error": "Failed to read file"})
        }

        doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(content)))
        if err != nil {
            return c.Status(500).JSON(fiber.Map{"error": "Failed to parse HTML"})
        }

        var originalContent string
        doc.Find("[data-content-id]").Each(func(i int, s *goquery.Selection) {
            if id, exists := s.Attr("data-content-id"); exists && id == req.ContentID {
                originalContent, _ = s.Html()
            }
        })

        if originalContent == "" {
            return c.Status(404).JSON(fiber.Map{"error": "Translation ID not found"})
        }

		// get the current translated content
		var currentTranslatedContent string
		doc.Find("[data-translation-id]").Each(func(i int, s *goquery.Selection) {
			if id, exists := s.Attr("data-translation-id"); exists && id == req.TranslationID {
				currentTranslatedContent, _ = s.Html()
			}
		})

		instructment := req.Instructions

		if len(currentTranslatedContent) > 0 {
			instructment = fmt.Sprintf("Previous translation:\n\n%s\n\n%s", currentTranslatedContent, instructment)
		}

		translatedContent, err := translateWithAI(originalContent, instructment, bookTitle)
        if err != nil {
            return c.Status(500).JSON(fiber.Map{"error": "Translation failed"})
        }

        return c.JSON(fiber.Map{"translated_content": translatedContent})
    })

	port := cmd.Flag("port").Value.String()

	slog.Info("- http://localhost:" + port + "/api/info")
	slog.Info("- http://localhost:" + port + "/toc.html")
	slog.Info("- http://localhost:" + port + "/api/manifest")
	slog.Info("- http://localhost:" + port + "/api/spine")

	return app.Listen(net.JoinHostPort("", port))
}
