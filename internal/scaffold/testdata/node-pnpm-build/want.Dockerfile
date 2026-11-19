# Written by kx deploy for a Node app.
FROM node:22-alpine
WORKDIR /app
COPY package.json pnpm-lock.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile
COPY . .
RUN npm run build
ENV PORT=8080 HOST=0.0.0.0
EXPOSE 8080
# Some hosts refuse containers that run as root, and can only check a numeric user.
USER 1000
CMD ["npm","start"]
