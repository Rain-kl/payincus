import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tsconfigPaths from "vite-tsconfig-paths";
import * as dotenv from "dotenv";
import * as fs from "fs";

dotenv.config({ path: ".env", quiet: true });
if (fs.existsSync(".env.local")) {
  dotenv.config({ path: ".env.local" });
}

const scssSettings = fs.readFileSync("src/sass/_settings.scss", "utf-8").trim();

export default defineConfig({
  base: "/incus-ui/",
  css: {
    preprocessorOptions: {
      scss: {
        api: "modern-compiler",
        silenceDeprecations: ["global-builtin", "import", "if-function"],
        additionalData: scssSettings,
      },
    },
  },
  plugins: [tsconfigPaths(), react()],
  build: {
    outDir: "./dist",
    minify: "esbuild",
    sourcemap: false,
  },
  define: {
    __UI_GIT_HASH__: JSON.stringify("payincus"),
  },
});
