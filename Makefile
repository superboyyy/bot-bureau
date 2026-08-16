.PHONY: test check test-backend test-frontend test-e2e build

test: test-backend test-frontend

check: test
	cd backend && go vet ./...

test-backend:
	cd backend && go test ./...

test-frontend:
	cd app && npm run test:unit

test-e2e:
	cd app && npm run build:backend && BOTBUREAU_RUN_E2E=1 npm run test:e2e

build:
	cd app && npm run build:backend
