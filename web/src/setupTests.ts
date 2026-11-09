import "@testing-library/jest-dom/vitest";
import { afterEach } from "vitest";

// the shared namespace choice lives in sessionStorage: start every test clean
afterEach(() => sessionStorage.clear());
