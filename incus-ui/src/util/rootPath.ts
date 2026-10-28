import { initPayIncusBridge } from "lib/payincus-bridge";

declare global {
  interface Window {
    ROOT_PATH: string;
  }
}

initPayIncusBridge();

export const ROOT_PATH =
  typeof window !== "undefined" ? window?.ROOT_PATH || "" : "";
