// The app uses <BrowserRouter>, where navigate() returns nothing. Without this
// overload every `onClick={() => navigate(…)}` reads as a dropped promise to
// no-floating-promises / no-misused-promises. Remove it if the app moves to a
// data router (createBrowserRouter), where navigate() does return one.
import "react-router";

declare module "react-router" {
  interface NavigateFunction {
    (to: To, options?: NavigateOptions): void;
    (delta: number): void;
  }
}
