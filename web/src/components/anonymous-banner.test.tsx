import { render, screen } from "@testing-library/react";
import { AnonymousBanner } from "@/components/anonymous-banner";

test("shows the anonymous-mode warning", () => {
  render(<AnonymousBanner />);
  expect(screen.getByRole("alert")).toHaveTextContent(/anonymous mode/i);
  expect(screen.getByRole("alert")).toHaveTextContent(/do not use in production/i);
});
