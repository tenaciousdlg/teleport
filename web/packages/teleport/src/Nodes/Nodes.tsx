/*
Copyright 2019-2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

import React from 'react';
import { Box, Indicator } from 'design';

import {
  FeatureBox,
  FeatureHeader,
  FeatureHeaderTitle,
} from 'teleport/components/Layout';
import Empty, { EmptyStateInfo } from 'teleport/components/Empty';
import NodeList from 'teleport/components/NodeList';
import ErrorMessage from 'teleport/components/AgentErrorMessage';
import useTeleport from 'teleport/useTeleport';
import AgentButtonAdd from 'teleport/components/AgentButtonAdd';
import cfg from 'teleport/config';
import history from 'teleport/services/history/history';
import localStorage from 'teleport/services/localStorage';

import { SearchResource } from 'teleport/Discover/SelectResource';

import { State, useNodes } from './useNodes';

export default function Container() {
  const teleCtx = useTeleport();
  const state = useNodes(teleCtx);
  return <Nodes {...state} />;
}

export function Nodes(props: State) {
  const {
    fetchedData,
    getNodeLoginOptions,
    startSshSession,
    attempt,
    canCreate,
    isLeafCluster,
    clusterId,
    fetchNext,
    fetchPrev,
    params,
    pageSize,
    setParams,
    setSort,
    pathname,
    replaceHistory,
    fetchStatus,
    isSearchEmpty,
    pageIndicators,
    onLabelClick,
  } = props;

  function onLoginSelect(e: React.MouseEvent, login: string, serverId: string) {
    e.preventDefault();
    startSshSession(login, serverId);
  }

  const hasNoNodes =
    attempt.status === 'success' &&
    fetchedData.agents.length === 0 &&
    isSearchEmpty;

  const enabled = localStorage.areUnifiedResourcesEnabled();
  if (enabled) {
    history.replace(cfg.getUnifiedResourcesRoute(clusterId));
  }

  return (
    <FeatureBox>
      <FeatureHeader alignItems="center" justifyContent="space-between">
        <FeatureHeaderTitle>Servers</FeatureHeaderTitle>
        {attempt.status === 'success' && !hasNoNodes && (
          <AgentButtonAdd
            agent={SearchResource.SERVER}
            beginsWithVowel={false}
            isLeafCluster={isLeafCluster}
            canCreate={canCreate}
          />
        )}
      </FeatureHeader>
      {attempt.status === 'failed' && (
        <ErrorMessage message={attempt.statusText} />
      )}
      {attempt.status === 'processing' && (
        <Box textAlign="center" m={10}>
          <Indicator />
        </Box>
      )}
      {attempt.status !== 'processing' && !hasNoNodes && (
        <NodeList
          nodes={fetchedData.agents}
          onLoginMenuOpen={getNodeLoginOptions}
          onLoginSelect={onLoginSelect}
          fetchNext={fetchNext}
          fetchPrev={fetchPrev}
          fetchStatus={fetchStatus}
          pageSize={pageSize}
          pageIndicators={pageIndicators}
          params={params}
          setParams={setParams}
          setSort={setSort}
          pathname={pathname}
          replaceHistory={replaceHistory}
          onLabelClick={onLabelClick}
        />
      )}
      {attempt.status === 'success' && hasNoNodes && (
        <Empty
          clusterId={clusterId}
          canCreate={canCreate && !isLeafCluster}
          emptyStateInfo={emptyStateInfo}
        />
      )}
    </FeatureBox>
  );
}

const emptyStateInfo: EmptyStateInfo = {
  title: 'Add your first server to Teleport',
  byline:
    'Teleport Server Access consolidates SSH access across all environments.',
  docsURL: 'https://goteleport.com/docs/server-access/getting-started/',
  resourceType: SearchResource.SERVER,
  readOnly: {
    title: 'No Servers Found',
    resource: 'servers',
  },
};
