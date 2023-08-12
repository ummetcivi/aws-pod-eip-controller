// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package handler

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws-samples/aws-pod-eip-controller/pkg/service"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type Handler struct {
	ChannelSize    int32
	EC2Service     *service.EC2Service
	ShiedService   *service.ShiedService
	ProcessChannel []chan event
	EipStatusMap   []map[string]event
}

func (h *Handler) init() {
	h.ProcessChannel = make([]chan event, h.ChannelSize)
	for i := 0; i < int(h.ChannelSize); i++ {
		h.ProcessChannel[i] = make(chan event, 100)
	}
	h.EipStatusMap = make([]map[string]event, h.ChannelSize)
	for i := 0; i < int(h.ChannelSize); i++ {
		h.EipStatusMap[i] = make(map[string]event)
		go h.process(i)
	}
}

func (h *Handler) process(i int) {
	var e event
	for e = range h.ProcessChannel[i] {
		logrus.WithFields(logrus.Fields{
			"event": e,
		}).Info("process event")
		val, ok := h.EipStatusMap[i][e.PodIP]
		if ok && val.ResourceVersion == e.ResourceVersion {
			logrus.Info("same resource version")
			continue
		}
		if !ok {
			err := e.Process(nil, h.EC2Service, h.ShiedService)
			if err != nil {
				logrus.Error(err)
				continue
			}
		} else {
			e.Process(&val, h.EC2Service, h.ShiedService)
		}
		h.EipStatusMap[i][e.PodIP] = e
	}
}

func (h *Handler) insert2Queue(event event) {
	hash := int32(event.PodIP2Int()) % h.ChannelSize
	h.ProcessChannel[hash] <- event
	logrus.WithFields(logrus.Fields{
		"event": event,
		"has":   hash,
	}).Info("insert event to queue")
}

func (h *Handler) HandleEvent(obj *unstructured.Unstructured, oldObj *unstructured.Unstructured, action string) (err error) {
	phase, exist, err := unstructured.NestedString(obj.Object, "status", "phase")
	if err != nil {
		return
	}
	if exist && phase != "Running" {
		logrus.Info("phase: ", phase)
		return
	}
	podIP, exist, err := unstructured.NestedString(obj.Object, "status", "podIP")
	if err != nil {
		return
	}
	if exist && len(podIP) == 0 {
		logrus.Info("podIP is empty")
		return
	}
	logrus.WithFields(logrus.Fields{
		"name":             obj.GetName(),
		"uid":              obj.GetUID(),
		"resource_version": obj.GetResourceVersion(),
		"annotions":        obj.GetAnnotations(),
		"phase":            phase,
		"podIP":            podIP,
		"action":           action,
	}).Info()
	event := event{
		PodIP:           podIP,
		ResourceVersion: obj.GetResourceVersion(),
		AttachIP:        false,
		ShiedAdv:        false,
	}
	switch action {
	case "update":
		annotations := obj.GetAnnotations()
		oldAnnotations := oldObj.GetAnnotations()
		if val, ok := annotations["service.beta.kubernetes.io/aws-eip-pod-controller-type"]; ok && val == "auto" {
			event.AttachIP = true
			if val, ok := annotations["service.beta.kubernetes.io/aws-eip-pod-controller-shield"]; ok && val == "advanced" {
				event.ShiedAdv = true
			}
		} else {
			if val, ok := oldAnnotations["service.beta.kubernetes.io/aws-eip-pod-controller-type"]; ok && val == "auto" {
				event.AttachIP = false
			} else {
				logrus.WithFields(logrus.Fields{
					"newobj": obj,
					"oldobj": oldObj,
				}).Info("ignore update event")
				return
			}
		}
	case "delete":
		event.ShiedAdv = false
		event.AttachIP = false
	}
	h.insert2Queue(event)
	return
}

func NewHandler(channelSize int32, vpcid string, region string) (handler *Handler, err error) {
	if len(vpcid) == 0 || len(region) == 0 {
		vpcid, region, err = getInfo()
		if err != nil {
			return nil, err
		}
	}
	ec2Service, err := service.NewEC2Service(vpcid, region)
	if err != nil {
		return nil, err
	}
	shieldService, err := service.NewShieldService(vpcid, region)
	if err != nil {
		return nil, err
	}
	handler = &Handler{
		ChannelSize:  channelSize,
		EC2Service:   ec2Service,
		ShiedService: shieldService,
	}
	handler.init()
	return handler, nil
}

func getInfo() (vpcid string, region string, err error) {
	// get vpcid from instance meta url
	url := "http://instance-data/latest/meta-data/network/interfaces/macs/"
	client := &http.Client{
		Timeout: time.Second * 5,
	}
	res, err := client.Get(url)
	if err != nil {
		return
	}
	macs, err := io.ReadAll(res.Body)
	if err != nil {
		return
	}
	mac := strings.Split(string(macs), "\n")[0]
	url = url + string(mac) + "/vpc-id"
	client.Get(url)
	if err != nil {
		return
	}
	res, err = client.Get(url)
	if err != nil {
		return
	}
	vpcID, err := io.ReadAll(res.Body)
	if err != nil {
		return
	}
	vpcid = string(vpcID)
	// get region from instance meta url
	url = "http://instance-data/latest/dynamic/instance-identity/document"
	client = &http.Client{
		Timeout: time.Second * 5,
	}
	res, err = client.Get(url)
	if err != nil {
		return
	}
	document, err := io.ReadAll(res.Body)
	if err != nil {
		return
	}
	region = gjson.Get(string(document), "region").String()

	logrus.WithFields(logrus.Fields{
		"vpcid":  vpcid,
		"region": region,
	}).Info("get info from imds")
	return
}