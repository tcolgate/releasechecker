package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gogo/protobuf/proto"
	pbr "github.com/tcolgate/helmreleaseupgrader/pkg/proto/hapi/release"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/tools/clientcmd"
)

type vk struct {
	v string
	k string
}

var rewrites = map[vk]vk{
	{v: "apps/v1beta1", k: "Deployment"}:       {v: "apps/v1", k: "Deployment"},
	{v: "apps/v1beta2", k: "Deployment"}:       {v: "apps/v1", k: "Deployment"},
	{v: "extensions/v1beta1", k: "Deployment"}: {v: "apps/v1", k: "Deployment"},
	{v: "batch/v1beta1", k: "CronJob"}:         {v: "batch/v1", k: "CronJob"},
	{v: "extensions/v1beta1", k: "Ingress"}:    {v: "networking.k8s.io/v1beta1", k: "Ingress"},
}

func getFromMap(node *yaml.Node, name, def string) string {
	for i := 0; i < len(node.Content); i += 2 {
		k := node.Content[i]
		v := node.Content[i+1]
		if k.Value != name {
			continue
		}
		return v.Value
	}

	return def
}

func updateNode(node *yaml.Node, defaultNS string) {
	for _, n := range node.Content {
		apiVersionIndex := -1
		apiVersion := ""
		kindIndex := -1
		kind := ""
		name := ""
		namespace := ""
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			v := n.Content[i+1]
			switch k.Value {
			case "apiVersion":
				apiVersionIndex = i + 1
				apiVersion = v.Value
			case "kind":
				kindIndex = i + 1
				kind = v.Value
			case "metadata":
				name = getFromMap(v, "name", "")
				namespace = getFromMap(v, "namespace", defaultNS)
			}

			if remap, ok := rewrites[vk{
				v: apiVersion,
				k: kind,
			}]; ok {
				fmt.Printf("remap %v/%v apiVersion: %v kind: %v to apiVersion: %v kind: %v \n", namespace, name, apiVersion, kind, remap.v, remap.k)
				n.Content[apiVersionIndex].Value = remap.v
				n.Content[kindIndex].Value = remap.k
			}
		}
	}
}

func rewriteManifest(in string, defaultNS string) (string, error) {
	dec := yaml.NewDecoder(bytes.NewBuffer([]byte(in)))

	out := &bytes.Buffer{}

	n := 0
	for {
		node := yaml.Node{}
		err := dec.Decode(&node)
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		updateNode(&node, defaultNS)
		if n != 0 {
			fmt.Fprintln(out, "---")
		}
		w := yaml.NewEncoder(out)
		err = w.Encode(&node)
		w.Close()
		if err != nil {
			return "", err
		}
		n++
	}

	return out.String(), nil
}

func main() {
	var kubeconfig *string
	if home := os.Getenv("HOME"); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatalf((err.Error()))
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf((err.Error()))
	}

	cms, err := clientset.CoreV1().ConfigMaps("").List(metav1.ListOptions{LabelSelector: "OWNER=TILLER,STATUS=DEPLOYED"})
	if err != nil {
		log.Fatalf((err.Error()))
	}

	for _, cm := range cms.Items {
		vstr := cm.Labels["VERSION"]
		_, err := strconv.Atoi(vstr)
		if err != nil {
			log.Printf("configmap %v has bad version, %v", cm.Name, err)
			continue
		}

		data, ok := cm.Data["release"]
		if !ok {
			log.Printf("configmap %v missing release", cm.Name)
			continue
		}
		zbs, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			log.Printf("could not base64 decode release %v, %v", cm.Name, err)
			continue
		}

		zbsr := bytes.NewBuffer(zbs)

		zr, err := gzip.NewReader(zbsr)
		if err != nil {
			log.Printf("could not decompress release %v, %v", cm.Name, err)
			continue
		}

		bs := &bytes.Buffer{}
		_, err = io.Copy(bs, zr)
		if err != nil {
			log.Printf("could not copy decompressed release %v, %v", cm.Name, err)
			continue
		}

		r := pbr.Release{}

		err = proto.Unmarshal(bs.Bytes(), &r)
		if err != nil {
			log.Printf("could not proto decode release %v, %v", cm.Name, err)
			continue
		}

		_, err = rewriteManifest(r.Manifest, cm.GetNamespace())
		if err != nil {
			log.Printf("could not rewrite manifest %v, %v", cm.Name, err)
			continue
		}

		/*
			outbs, err := proto.Marshal(&r)
			if err != nil {
				log.Printf("could not marshal manifest %v, %v", cm.Name, err)
				continue
			}

			outzbs := &bytes.Buffer{}
			zw := gzip.NewWriter(outzbs)
			_, err = io.Copy(zw, bytes.NewBuffer(outbs))
			if err != nil {
				log.Printf("failed recompressing manifest %v, %v", cm.Name, err)
				continue
			}
			zw.Close()

			zstr := base64.StdEncoding.EncodeToString(outzbs.Bytes())

			newcm := cm.DeepCopy()
			newcm.Labels["VERSION"] = strconv.Itoa(version + 1)
			newcm.Data["release"] = zstr

			updatecm := cm.DeepCopy()
			updatecm.Labels["STATUS"] = "S"

			fmt.Print(out)
		*/
	}
}
