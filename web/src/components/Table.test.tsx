import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { Table } from "./Table";
import type { TableColumn } from "./Table";

interface Row {
  id: string;
  name: string;
  status: string;
}

const COLS: TableColumn<Row>[] = [
  { key: "id",     header: "ID",     render: (r) => r.id },
  { key: "name",   header: "Name",   render: (r) => r.name },
  { key: "status", header: "Status", render: (r) => r.status },
];

const ROWS: Row[] = [
  { id: "abc", name: "Alpha", status: "running" },
  { id: "def", name: "Beta",  status: "stopped" },
];

describe("Table", () => {
  it("renders column headers", () => {
    render(<Table columns={COLS} rows={ROWS} rowKey={(r) => r.id} />);
    expect(screen.getByText("ID")).toBeTruthy();
    expect(screen.getByText("Name")).toBeTruthy();
    expect(screen.getByText("Status")).toBeTruthy();
  });

  it("renders all row data", () => {
    render(<Table columns={COLS} rows={ROWS} rowKey={(r) => r.id} />);
    expect(screen.getByText("Alpha")).toBeTruthy();
    expect(screen.getByText("Beta")).toBeTruthy();
    expect(screen.getByText("running")).toBeTruthy();
    expect(screen.getByText("stopped")).toBeTruthy();
  });

  it("renders empty state when rows is empty", () => {
    render(
      <Table
        columns={COLS}
        rows={[]}
        rowKey={(r) => r.id}
        empty={<span>Nothing here</span>}
      />,
    );
    expect(screen.getByText("Nothing here")).toBeTruthy();
  });

  it("renders correct number of data rows", () => {
    const { container } = render(
      <Table columns={COLS} rows={ROWS} rowKey={(r) => r.id} />,
    );
    const tbody = container.querySelector("tbody");
    expect(tbody?.querySelectorAll("tr").length).toBe(2);
  });

  it("uses rowKey for react keys (no duplicate key warnings)", () => {
    // If rowKey is called, each row gets a unique key; just verify it renders
    const { container } = render(
      <Table columns={COLS} rows={ROWS} rowKey={(r) => r.id} />,
    );
    expect(container.querySelectorAll("tr").length).toBe(3); // 1 header + 2 data
  });

  it("does not render a sort button for non-sortable columns (opt-in, no regression)", () => {
    const { container } = render(<Table columns={COLS} rows={ROWS} rowKey={(r) => r.id} />);
    expect(container.querySelector(".th-sort-btn")).toBeNull();
  });

  it("renders a sort button + aria-sort only for columns marked sortable", () => {
    const sortableCols: TableColumn<Row>[] = [
      { key: "id", header: "ID", render: (r) => r.id },
      { key: "name", header: "Name", render: (r) => r.name, sortable: true },
    ];
    const { container } = render(
      <Table columns={sortableCols} rows={ROWS} rowKey={(r) => r.id} sortKey={null} onSort={() => {}} />,
    );
    const ths = container.querySelectorAll("th");
    expect(ths[0].getAttribute("aria-sort")).toBeNull();
    expect(ths[1].getAttribute("aria-sort")).toBe("none");
    expect(ths[1].querySelector(".th-sort-btn")).not.toBeNull();
  });

  it("calls onSort with the column key when a sortable header is clicked", () => {
    const onSort = vi.fn();
    const sortableCols: TableColumn<Row>[] = [
      { key: "name", header: "Name", render: (r) => r.name, sortable: true },
    ];
    render(
      <Table columns={sortableCols} rows={ROWS} rowKey={(r) => r.id} sortKey={null} onSort={onSort} />,
    );
    fireEvent.click(screen.getByText("Name"));
    expect(onSort).toHaveBeenCalledWith("name");
  });

  it("shows the active sort direction indicator", () => {
    const sortableCols: TableColumn<Row>[] = [
      { key: "name", header: "Name", render: (r) => r.name, sortable: true },
    ];
    const { container, rerender } = render(
      <Table columns={sortableCols} rows={ROWS} rowKey={(r) => r.id} sortKey="name" sortDir="asc" onSort={() => {}} />,
    );
    expect(container.querySelector("th")?.getAttribute("aria-sort")).toBe("ascending");
    expect(container.querySelector(".th-sort-ind")?.textContent).toBe("▲");

    rerender(
      <Table columns={sortableCols} rows={ROWS} rowKey={(r) => r.id} sortKey="name" sortDir="desc" onSort={() => {}} />,
    );
    expect(container.querySelector("th")?.getAttribute("aria-sort")).toBe("descending");
    expect(container.querySelector(".th-sort-ind")?.textContent).toBe("▼");
  });

  it("applies rowStyle per row when provided (opt-in)", () => {
    const { container } = render(
      <Table
        columns={COLS}
        rows={ROWS}
        rowKey={(r) => r.id}
        rowStyle={(r) => (r.status === "stopped" ? { opacity: 0.62 } : undefined)}
      />,
    );
    const rows = container.querySelectorAll("tbody tr");
    expect((rows[0] as HTMLElement).style.opacity).toBe("");
    expect((rows[1] as HTMLElement).style.opacity).toBe("0.62");
  });
});
