.PHONY: dev dev-frontend dev-backend canary

# Start the complete local development stack (backend, user frontend, admin frontend).
dev:
	@pnpm dev

# Start both frontend entry points without the backend.
dev-frontend:
	@pnpm exec concurrently -n "user,admin" -c "green,magenta" \
		"VITE_APP_ENTRY=user VITE_DEV_PORT=3000 VITE_DEV_PROXY_TARGET=http://127.0.0.1:3001 VITE_ADMIN_BASE_URL=http://127.0.0.1:3002 pnpm --filter client dev:user" \
		"VITE_APP_ENTRY=admin VITE_CUSTOMER_BASE_URL=http://127.0.0.1:3000 VITE_DEV_PORT=3002 VITE_DEV_PROXY_TARGET=http://127.0.0.1:3001 pnpm --filter client dev:admin"

# Start the backend only in local development mode.
dev-backend:
	@pnpm --filter server dev

# Merge the current local branch into canary, push canary, then restore it.
# Dirty worktrees are auto-stashed and restored after the operation.
canary:
	@set -e; \
	if ! git rev-parse --git-dir >/dev/null 2>&1; then \
		echo "Error: not a git repository"; exit 1; \
	fi; \
	orig=$$(git rev-parse --abbrev-ref HEAD); \
	if [ "$$orig" = "HEAD" ]; then \
		echo "Error: detached HEAD; checkout a branch first"; exit 1; \
	fi; \
	if [ "$$orig" = "canary" ]; then \
		echo "Error: already on canary; checkout a source branch first"; exit 1; \
	fi; \
	stashed=0; \
	if [ -n "$$(git status --porcelain)" ]; then \
		echo "Working tree is dirty; stashing local changes..."; \
		git stash push -u -m "make canary auto-stash from $$orig"; \
		stashed=1; \
	fi; \
	cleanup() { \
		cur=$$(git rev-parse --abbrev-ref HEAD 2>/dev/null || true); \
		if [ "$$cur" != "$$orig" ]; then \
			echo "Restoring branch $$orig..."; \
			git checkout -q "$$orig"; \
		fi; \
		if [ "$$stashed" = "1" ]; then \
			echo "Restoring stashed local changes..."; \
			git stash pop; \
		fi; \
	}; \
	trap cleanup EXIT; \
	echo "Merging $$orig -> canary..."; \
	git checkout canary; \
	git merge --no-edit "$$orig"; \
	git push -u origin canary; \
	echo "Done: $$orig merged into canary and pushed."

# The control repository consumes the Incus node distribution; it does not build a host agent.
.PHONY: check
check:
	@node --import ./server/node_modules/tsx/dist/loader.mjs server/scripts/test-node-distribution.ts
