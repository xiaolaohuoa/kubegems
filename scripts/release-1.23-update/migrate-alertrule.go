// Copyright 2023 The kubegems.io Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1alpha1"
	"github.com/stoewer/go-strcase"
	"gopkg.in/yaml.v3"
	v1 "k8s.io/api/core/v1"
	"kubegems.io/kubegems/pkg/apis/gems"
	"kubegems.io/kubegems/pkg/log"
	"kubegems.io/kubegems/pkg/service/handlers/observability"
	"kubegems.io/kubegems/pkg/service/models"
	"kubegems.io/kubegems/pkg/service/observe"
	"kubegems.io/kubegems/pkg/utils/agents"
	"kubegems.io/kubegems/pkg/utils/database"
	"kubegems.io/kubegems/pkg/utils/prometheus"
	"kubegems.io/kubegems/pkg/utils/prometheus/promql"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func exportOldAlertRulesToDB(ctx context.Context, cs *agents.ClientSet, db *database.Database) ([]*models.AlertRule, error) {
	alertrules := []*models.AlertRule{}
	if err := cs.ExecuteInEachCluster(ctx, func(ctx context.Context, cli agents.Client) error {
		observecli := observe.NewClient(cli, db.DB())
		monitorAlertRules, err := observecli.ListMonitorAlertRules(ctx, "", false, models.NewPromqlTplMapperFromFile().FindPromqlTpl)
		if err != nil {
			log.Errorf("ListMonitorAlertRules in cluster: %s, err: %v", cli.Name(), err)
		}
		loggingAlertRules, err := observecli.ListLoggingAlertRules(ctx, "", false)
		if err != nil {
			log.Errorf("ListLoggingAlertRules in cluster: %s, err: %v", cli.Name(), err)
		}
		for _, v := range monitorAlertRules {
			alertrules = append(alertrules, convertMonitorAlertRule(cli.Name(), v))
		}
		for _, v := range loggingAlertRules {
			alertrules = append(alertrules, convertLoggingAlertRule(cli.Name(), v))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, v := range alertrules {
		if err := observability.SetReceivers(v, db.DB()); err != nil {
			log.Errorf("SetReceivers for: %s failed: %v", v.FullName(), err)
			continue
		}
		if err := db.DB().Omit("Receivers.AlertChannel").Create(v).Error; err != nil {
			log.Errorf("create alertrule %s in db failed: %v", v.FullName(), err)
		}
		log.Info("export alertrule success", "name", v.FullName())
	}
	return alertrules, nil
}

// update alertname map in file
func updateAlertNameMapInFile(db *database.Database) error {
	alertNameMap := map[string]string{}
	f, err := os.OpenFile(alertNameMapFile, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	if err := yaml.NewDecoder(f).Decode(alertNameMap); err != nil && err != io.EOF {
		return err
	}
	oldnames := []string{}
	if err := db.DB().Raw(`SELECT name FROM slt.alert_rules where length(name) != char_length(name) or name like "% %"`).Scan(&oldnames).Error; err != nil {
		return err
	}
	for _, v := range oldnames {
		if _, ok := alertNameMap[v]; !ok {
			alertNameMap[v] = ""
			log.Info("add new alert name", "name", v)
		}
	}

	bts, _ := yaml.Marshal(alertNameMap)
	return os.WriteFile(alertNameMapFile, bts, 0644)
}

type alertruleInfo struct {
	OldName string
	NewName string

	database.EnvInfo
}

// update alert rule name in db
func updateAlertRuleNameInDB(db *database.Database) error {
	alertNameMap := map[string]string{}
	f, err := os.OpenFile(alertNameMapFile, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	if err := yaml.NewDecoder(f).Decode(alertNameMap); err != nil {
		return err
	}
	alertrules := []*models.AlertRule{}
	if err := db.DB().Find(&alertrules).Error; err != nil {
		return err
	}

	envinfo, err := db.ClusterNS2EnvMap()
	if err != nil {
		return err
	}
	records := make([][]string, 0, len(alertrules))
	for _, v := range alertrules {
		newname, ok := alertNameMap[v.Name]
		if !ok {
			newname = strcase.KebabCase(v.Name)
		}
		if err := models.IsValidAlertRuleName(newname); err != nil {
			return err
		}

		info := envinfo[fmt.Sprintf("%s/%s", v.Cluster, v.Namespace)]
		records = append(records, []string{
			v.Name, newname, info.ClusterName, info.Namespace,
			info.TenantName, info.ProjectName, info.EnvironmentName,
		})

		if v.Name != newname {
			log.Info("update alertrule name", "alertrule", v.FullName(), "newname", newname)
			if err := db.DB().Model(v).Update("name", newname).Error; err != nil {
				return errors.Wrapf(err, "update alertrule: %s name", v.FullName())
			}
		}
	}

	file, _ := os.OpenFile(alertNameChangeRecordFile, os.O_CREATE|os.O_RDWR, 0644)
	defer file.Close()
	file.Write([]byte{0xEF, 0xBB, 0xBF})
	w := csv.NewWriter(file)
	w.Write([]string{"oldname", "newname", "cluster", "namespace", "tenant_name", "project_name", "environment_name"})
	w.WriteAll(records)

	time.Sleep((5 * time.Second))
	return nil
}

func deleteK8sAlertRuleCfgs(ctx context.Context, cs *agents.ClientSet) error {
	return cs.ExecuteInEachCluster(ctx, func(ctx context.Context, cli agents.Client) error {
		amList := v1alpha1.AlertmanagerConfigList{}
		err := cli.List(ctx, &amList, client.InNamespace(v1.NamespaceAll), client.HasLabels([]string{gems.LabelAlertmanagerConfigName}))
		if err != nil {
			log.Errorf("list cmcfg in cluster: %s failed", cli.Name())
		}
		for _, v := range amList.Items {
			if err := cli.Delete(ctx, v); err != nil {
				log.Errorf("delete alertmanager config: %s/%s in cluster: %s failed", cli.Name(), v.Namespace, v.Name)
			}
		}

		ruleList := monitoringv1.PrometheusRuleList{}
		err = cli.List(ctx, &ruleList, client.InNamespace(v1.NamespaceAll),
			client.HasLabels([]string{gems.LabelPrometheusRuleName}),
			client.MatchingLabels(map[string]string{
				gems.LabelPrometheusRuleType: prometheus.AlertTypeMonitor,
			}),
		)
		if err != nil {
			log.Errorf("list cmcfg in cluster: %s failed", cli.Name())
		}
		for _, v := range ruleList.Items {
			if err := cli.Delete(ctx, v); err != nil {
				log.Errorf("delete prometheus rule: %s/%s in cluster: %s failed", cli.Name(), v.Namespace, v.Name)
			}
		}
		return nil
	})
}

func syncAlertRules(ctx context.Context, cs *agents.ClientSet, db *database.Database) error {
	alertrules := []*models.AlertRule{}
	if err := db.DB().Preload("Receivers.AlertChannel").Find(&alertrules).Error; err != nil {
		return err
	}
	for _, v := range alertrules {
		cli, err := cs.ClientOf(ctx, v.Cluster)
		if err != nil {
			log.Errorf("client of: %s failed", cli.Name())
			continue
		}
		if err := observability.NewAlertRuleProcessor(cli, db).SyncAlertRule(ctx, v); err != nil {
			log.Error(err, "SyncAlertRule")
		} else {
			log.Info("sync alert rule success", "name", v.FullName())
		}
	}
	return nil
}

func matchType(value string) promql.MatchType {
	if strings.Contains(value, "|") {
		return promql.MatchRegexp
	}
	return promql.MatchEqual
}

func convertMonitorAlertRule(cluster string, monitorRule observe.MonitorAlertRule) *models.AlertRule {
	ret := &models.AlertRule{
		Cluster:       cluster,
		Namespace:     monitorRule.Namespace,
		Name:          monitorRule.Name,
		AlertType:     prometheus.AlertTypeMonitor,
		Expr:          monitorRule.Expr,
		Message:       monitorRule.Message,
		For:           monitorRule.For,
		InhibitLabels: monitorRule.InhibitLabels,
		IsOpen:        monitorRule.IsOpen,
	}
	for _, level := range monitorRule.AlertLevels {
		ret.AlertLevels = append(ret.AlertLevels, models.AlertLevel{
			CompareOp:    level.CompareOp,
			CompareValue: level.CompareValue,
			Severity:     level.Severity,
		})
	}
	for _, rec := range monitorRule.Receivers {
		ret.Receivers = append(ret.Receivers, &models.AlertReceiver{
			AlertChannelID: rec.AlertChannel.ID,
			Interval:       rec.Interval,
		})
	}
	if monitorRule.PromqlGenerator != nil {
		ret.PromqlGenerator = &models.PromqlGenerator{
			Scope:    monitorRule.PromqlGenerator.Scope,
			Resource: monitorRule.PromqlGenerator.Resource,
			Rule:     monitorRule.PromqlGenerator.Rule,
			Unit:     monitorRule.PromqlGenerator.Unit,
		}
		for k, v := range monitorRule.PromqlGenerator.LabelPairs {
			ret.PromqlGenerator.LabelMatchers = append(ret.PromqlGenerator.LabelMatchers, promql.LabelMatcher{
				Type:  matchType(v),
				Name:  k,
				Value: v,
			})
		}
	}
	if monitorRule.ChannelStatus != observe.StatusNormal {
		log.Errorf("monitor alertrule: %s channel status: %d", ret.FullName(), monitorRule.ChannelStatus)
	}
	return ret
}

func convertLoggingAlertRule(cluster string, loggingRule observe.LoggingAlertRule) *models.AlertRule {
	ret := &models.AlertRule{
		Cluster:       cluster,
		Namespace:     loggingRule.Namespace,
		Name:          loggingRule.Name,
		AlertType:     prometheus.AlertTypeLogging,
		Expr:          loggingRule.Expr,
		Message:       loggingRule.Message,
		For:           loggingRule.For,
		InhibitLabels: loggingRule.InhibitLabels,
		IsOpen:        loggingRule.IsOpen,
	}
	for _, level := range loggingRule.AlertLevels {
		ret.AlertLevels = append(ret.AlertLevels, models.AlertLevel{
			CompareOp:    level.CompareOp,
			CompareValue: level.CompareValue,
			Severity:     level.Severity,
		})
	}
	for _, rec := range loggingRule.Receivers {
		ret.Receivers = append(ret.Receivers, &models.AlertReceiver{
			AlertChannelID: rec.AlertChannel.ID,
			Interval:       rec.Interval,
		})
	}
	if loggingRule.LogqlGenerator != nil {
		ret.LogqlGenerator = &models.LogqlGenerator{
			Duration: loggingRule.LogqlGenerator.Duration,
			Match:    loggingRule.LogqlGenerator.Match,
		}
		for k, v := range loggingRule.LogqlGenerator.LabelPairs {
			ret.LogqlGenerator.LabelMatchers = append(ret.LogqlGenerator.LabelMatchers, promql.LabelMatcher{
				Type:  matchType(v),
				Name:  k,
				Value: v,
			})
		}
	}
	if loggingRule.ChannelStatus != observe.StatusNormal {
		log.Errorf("logging alertrule: %s channel status: %d", ret.FullName(), loggingRule.ChannelStatus)
	}
	return ret
}
