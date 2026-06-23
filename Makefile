# Trento submodule workflow
#
# Common usage:
#   make status
#   make foreach CMD='git log -1 --oneline' # run a command in every submodule
#
# Per-submodule (web | wanda | agent | checks | contracts | helm-charts):
#   make commit-web MSG="fix X"             # commits already-staged changes in web/
#   make push-web                           # pushes web/ HEAD to its own origin
#
# Parent repo (after pushing in submodules):
#   make bump-pins MSG="bump submodules"    # stage & commit new submodule SHAs
#   make push-pins                          # push parent repo

SUBMODULES := web wanda agent checks contracts helm-charts

RESTATE_CONTAINER ?= trento-restate
RESTATE_IMAGE     ?= docker.restate.dev/restatedev/restate:latest
RESTATE_INGRESS   ?= 8080
RESTATE_ADMIN     ?= 9070

.PHONY: help status foreach bump-pins push-pins restate-up restate-down
.PHONY: restate-server-up restate-server-down handler-up handler-down wf-register
.PHONY: wf-run wf-send wf-attach wf-list wf-status wf-describe wf-cancel wf-kill
.PHONY: $(addprefix commit-,$(SUBMODULES))
.PHONY: $(addprefix push-,$(SUBMODULES))

help:
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z_%-]+:.*##/ {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## --- inspection ---

status: ## Show git status for parent and every submodule
	@echo "== parent =="
	@git status -sb
	@for s in $(SUBMODULES); do \
		echo; echo "== $$s =="; \
		git -C $$s status -sb; \
	done

foreach: ## Run CMD='...' in every submodule
	@test -n "$(CMD)" || { echo 'Usage: make foreach CMD="git log -1"'; exit 1; }
	@for s in $(SUBMODULES); do \
		echo "== $$s =="; \
		( cd $$s && $(CMD) ) || exit $$?; \
	done

## --- per-submodule commit / push ---
# Pattern targets: commit-<name> MSG="...", push-<name>
# Stage your files yourself (e.g. `git -C web add path/to/file`) before commit-*.

commit-%: ## Commit already-staged changes in <submodule> (MSG="...")
	@test -n "$(MSG)" || { echo 'Usage: make commit-$* MSG="message"'; exit 1; }
	git -C $* commit -m "$(MSG)"

push-%: ## Push <submodule> HEAD to its own origin (sets upstream)
	git -C $* push -u origin HEAD

## --- parent repo: bump submodule pins ---

bump-pins: ## Stage & commit updated submodule SHAs in the parent repo (MSG="...")
	@test -n "$(MSG)" || { echo 'Usage: make bump-pins MSG="message"'; exit 1; }
	git add $(SUBMODULES)
	git commit -m "$(MSG)"

push-pins: ## Push the parent repo HEAD to origin (sets upstream)
	git push -u origin HEAD

## --- restate-server (local workflow orchestrator) ---
# Container name, image, and ports are overridable via env or `make VAR=...`.
# Ingress = 8080 (curl POST <service>/<key>/<handler> to invoke workflows).
# Admin/UI = 9070 (http://localhost:9070 for the web console).

restate-up: restate-server-up handler-down handler-up wf-register ## Start restate-server (idempotent) + (re)install handler + register

restate-down: handler-down restate-server-down ## Stop handler and remove restate-server

restate-server-up: ## Start only the restate-server container (idempotent)
	@command -v docker >/dev/null 2>&1 || { echo "docker not found in PATH"; exit 1; }
	@if docker inspect $(RESTATE_CONTAINER) >/dev/null 2>&1; then \
		state=$$(docker inspect -f '{{.State.Status}}' $(RESTATE_CONTAINER)); \
		if [ "$$state" = "running" ]; then \
			echo "restate-server already running"; \
		else \
			echo "starting existing container $(RESTATE_CONTAINER) (was $$state)"; \
			docker start $(RESTATE_CONTAINER) >/dev/null; \
		fi; \
	else \
		echo "creating $(RESTATE_CONTAINER) from $(RESTATE_IMAGE)"; \
		docker run -d --name $(RESTATE_CONTAINER) \
			-p $(RESTATE_INGRESS):8080 -p $(RESTATE_ADMIN):9070 \
			--add-host=host.docker.internal:host-gateway \
			$(RESTATE_IMAGE) >/dev/null; \
	fi
	@# Wait until the admin API responds, so a follow-up `wf-register`
	@# doesn't race a freshly-created container's boot.
	@for i in $$(seq 1 30); do \
		docker exec $(RESTATE_CONTAINER) restate -y deployments list >/dev/null 2>&1 && break; \
		sleep 0.5; \
	done
	@echo "  ingress: http://localhost:$(RESTATE_INGRESS)"
	@echo "  admin:   http://localhost:$(RESTATE_ADMIN)"

restate-server-down: ## Stop and remove only the restate-server container
	@command -v docker >/dev/null 2>&1 || { echo "docker not found in PATH"; exit 1; }
	@if docker inspect $(RESTATE_CONTAINER) >/dev/null 2>&1; then \
		docker rm -f $(RESTATE_CONTAINER) >/dev/null; \
		echo "removed $(RESTATE_CONTAINER)"; \
	else \
		echo "$(RESTATE_CONTAINER) not present"; \
	fi

## --- workflow handler (the Go service that hosts the workflow code) ---
# Built into .workflows/.runs/handler and supervised by a pidfile.

WORKFLOWS_DIR  ?= .workflows
HANDLER_BIN     = $(WORKFLOWS_DIR)/.runs/handler
HANDLER_PID     = $(WORKFLOWS_DIR)/.runs/handler.pid
HANDLER_LOG     = $(WORKFLOWS_DIR)/.runs/handler.log
HANDLER_URL    ?= http://host.docker.internal:9080

handler-up: ## Build and start the workflow handler in background (idempotent)
	@if [ -s $(HANDLER_PID) ] && kill -0 "$$(cat $(HANDLER_PID))" 2>/dev/null; then \
		echo "handler already running (pid $$(cat $(HANDLER_PID)))"; \
	else \
		mkdir -p $(WORKFLOWS_DIR)/.runs && \
		go build -C $(WORKFLOWS_DIR) -o .runs/handler ./cmd/handler && \
		( TRENTO_REPO_ROOT=$(CURDIR) nohup $(HANDLER_BIN) > $(HANDLER_LOG) 2>&1 & echo $$! > $(HANDLER_PID) ) && \
		sleep 1 && \
		if kill -0 "$$(cat $(HANDLER_PID))" 2>/dev/null; then \
			echo "handler up (pid $$(cat $(HANDLER_PID)), log $(HANDLER_LOG))"; \
		else \
			echo "handler died on startup; tail of $(HANDLER_LOG):"; \
			tail -20 $(HANDLER_LOG); \
			exit 1; \
		fi; \
	fi

handler-down: ## Stop the workflow handler started by handler-up
	@if [ ! -s $(HANDLER_PID) ]; then \
		echo "no handler running"; \
	else \
		pid="$$(cat $(HANDLER_PID))"; \
		if kill -0 "$$pid" 2>/dev/null; then \
			kill "$$pid" && echo "stopped handler (pid $$pid)"; \
		else \
			echo "handler not running (stale pidfile)"; \
		fi; \
		rm -f $(HANDLER_PID); \
	fi

wf-register: handler-up ## Start the handler (if needed) and register it with restate-server
	@for i in 1 2 3 4 5 6 7 8 9 10; do \
		ss -tln 2>/dev/null | grep -q ':9080 ' && break; \
		sleep 0.3; \
	done
	@docker exec $(RESTATE_CONTAINER) restate -y deployments register $(HANDLER_URL) --force

## --- workflow run / inspect / cancel ---
# Generic wrappers for any registered workflow. WF is the short name
# (workflow id minus the `trento.` prefix); KEY is the per-workflow id
# (e.g. branch name for test-on-obs, "manual" for patch-release).
#
#   make wf-run    WF=test-on-obs    KEY=$$(git symbolic-ref --short HEAD) INPUT='{"fixIterate":"off","maxAttempts":3}'
#   make wf-send   WF=patch-release  KEY=manual INPUT='{}'
#   make wf-attach WF=test-on-obs    KEY=poc-branch
#   make wf-list   [WF=test-on-obs]
#   make wf-status WF=test-on-obs    KEY=poc-branch
#   make wf-cancel WF=test-on-obs    KEY=poc-branch
#   make wf-kill   WF=test-on-obs    KEY=poc-branch

WF      ?=
KEY     ?= run-$(shell date +%s)
INPUT   ?= {}
SERVICE  = trento.$(WF)

# Shared arg-validation snippet for targets that need both WF and KEY.
define _need_wf_key
	@test -n "$(WF)"  || { echo 'set WF=<workflow-short-name> (test-on-obs | patch-release)'; exit 1; }
	@test -n "$(KEY)" || { echo 'set KEY=<workflow-key>'; exit 1; }
endef

# Render JSON pretty when jq is available, otherwise pass through.
_FMT = (command -v jq >/dev/null 2>&1 && jq . || cat)

wf-run: ## Spawn a workflow async (returns invocation handle immediately): WF=<short> [KEY=<key>] [INPUT='<json>']
	$(call _need_wf_key)
	@echo "POST /$(SERVICE)/$(KEY)/Run/send  input=$(INPUT)"
	@curl -fsS -X POST \
		http://localhost:$(RESTATE_INGRESS)/$(SERVICE)/$(KEY)/Run/send \
		-H 'content-type: application/json' \
		-d '$(INPUT)' | $(_FMT)
	@echo "  attach: make wf-attach WF=$(WF) KEY=$(KEY)"

wf-send: ## Invoke a workflow async (returns an invocation handle): WF=<short> KEY=<key> [INPUT='<json>']
	$(call _need_wf_key)
	@curl -fsS -X POST \
		http://localhost:$(RESTATE_INGRESS)/$(SERVICE)/$(KEY)/Run/send \
		-H 'content-type: application/json' \
		-d '$(INPUT)' | $(_FMT)

wf-attach: ## Block until a previously-sent workflow completes, then print its result: WF=<short> KEY=<key>
	$(call _need_wf_key)
	@curl -fsS \
		http://localhost:$(RESTATE_INGRESS)/restate/workflow/$(SERVICE)/$(KEY)/attach | $(_FMT)

wf-list: ## List all invocations (incl. completed). Optional filter: WF=<short>
	@if [ -n "$(WF)" ]; then \
		docker exec $(RESTATE_CONTAINER) restate -y inv list --all --service "$(SERVICE)"; \
	else \
		docker exec $(RESTATE_CONTAINER) restate -y inv list --all; \
	fi

wf-status: ## Show one workflow's row(s) by service+key: WF=<short> KEY=<key>
	$(call _need_wf_key)
	@docker exec $(RESTATE_CONTAINER) restate -y inv list --all --service "$(SERVICE)" --key "$(KEY)"

wf-describe: ## Full journal of one invocation by id: ID=inv_xxx (find via wf-list)
	@test -n "$(ID)" || { echo 'set ID=inv_xxx (run `make wf-list` to find it)'; exit 1; }
	@docker exec $(RESTATE_CONTAINER) restate -y inv describe "$(ID)"

wf-cancel: ## Gracefully cancel a running workflow: WF=<short> KEY=<key>
	$(call _need_wf_key)
	@docker exec $(RESTATE_CONTAINER) restate -y inv cancel "$(SERVICE)/$(KEY)"

wf-kill: ## Force-kill a workflow (no cleanup): WF=<short> KEY=<key>
	$(call _need_wf_key)
	@docker exec $(RESTATE_CONTAINER) restate -y inv kill "$(SERVICE)/$(KEY)"

## --- fix-pr-ci dedicated wrapper ---
# Watches a PR's CI, classifies failures (flaky | bug | infra | unfixable),
# reruns flakies, asks Claude to fix bugs autonomously, commits fixups,
# pushes, and loops until green or exhausted. Defaults: MaxAttempts=5,
# MaxIterations=15, CleanupOnExit=false. To override, invoke wf-run
# directly with a richer INPUT.

.PHONY: fix-pr-ci

fix-pr-ci: ## Watch a PR's CI and fix iteratively. REPO=owner/name PR=N
	@test -n "$(REPO)" || { echo 'usage: make fix-pr-ci REPO=owner/name PR=<number>'; exit 1; }
	@test -n "$(PR)"   || { echo 'usage: make fix-pr-ci REPO=owner/name PR=<number>'; exit 1; }
	@$(MAKE) wf-run WF=fix-pr-ci KEY=$(subst /,_,$(REPO))_pr$(PR) \
		INPUT='{"repo":"$(REPO)","prNumber":$(PR)}'
