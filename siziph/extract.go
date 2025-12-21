package siziph

import (
    "fmt"
    "io"
    "net/url"
    "os"
    "path/filepath"
    "strings"
    "encoding/json"
    "encoding/xml"
    "io/ioutil"

    zip "github.com/yeka/zip"
)

func Extract(input, output string) error {
    r, err := zip.OpenReader(input)
    if err != nil {
        return fmt.Errorf("open siq: %w", err)
    }
    defer r.Close()

    for _, f := range r.File {
        name, err := prepareName(f.Name)
        if err != nil {
            return err
        }

        outPath := filepath.Join(output, name)

        if f.FileInfo().IsDir() {
            os.MkdirAll(outPath, os.ModePerm)
            continue
        }

        os.MkdirAll(filepath.Dir(outPath), os.ModePerm)

        rc, err := f.Open()
        if err != nil {
            return err
        }

        outFile, err := os.Create(outPath)
        if err != nil {
            rc.Close()
            return err
        }

        _, err = io.Copy(outFile, rc)
        outFile.Close()
        rc.Close()

        if err != nil {
            return err
        }

        // Convert XML to JSON automatically
        if strings.HasSuffix(strings.ToLower(outPath), ".xml") {
            if err := convertXMLtoJSON(outPath); err != nil {
                fmt.Println("Warning: JSON conversion failed:", err)
            }
        }
    }

    return nil
}

func prepareName(name string) (string, error) {
    decoded, err := url.QueryUnescape(name)
    if err != nil {
        return "", err
    }

    invalid := []string{":", "*", "?", "\"", "<", ">", "|", "%"}
    for _, ch := range invalid {
        decoded = strings.ReplaceAll(decoded, ch, "_")
    }

    return decoded, nil
}

// convert to json

type xmlNode struct {
    XMLName xml.Name
    Attrs   []xml.Attr `xml:",any,attr"`
    Nodes   []xmlNode  `xml:",any"`
    Content string     `xml:",chardata"`
}

func xmlToMap(n xmlNode) map[string]interface{} {
    m := map[string]interface{}{}

    // attributes
    for _, a := range n.Attrs {
        m["@"+a.Name.Local] = a.Value
    }

    // text
    if strings.TrimSpace(n.Content) != "" {
        m["#text"] = strings.TrimSpace(n.Content)
    }

    // child nodes
    for _, child := range n.Nodes {
        key := child.XMLName.Local
        val := xmlToMap(child)

        if existing, ok := m[key]; ok {
            m[key] = append(existing.([]interface{}), val)
        } else {
            m[key] = []interface{}{val}
        }
    }

    return m
}

func convertXMLtoJSON(xmlPath string) error {
    xmlData, err := ioutil.ReadFile(xmlPath)
    if err != nil {
        return err
    }

    var root xmlNode
    if err := xml.Unmarshal(xmlData, &root); err != nil {
        return err
    }

    jsonData, err := json.MarshalIndent(xmlToMap(root), "", "  ")
    if err != nil {
        return err
    }

    jsonPath := xmlPath[:len(xmlPath)-len(filepath.Ext(xmlPath))] + ".json"
    return ioutil.WriteFile(jsonPath, jsonData, 0644)
}