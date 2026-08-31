// /app/account/devices — everything holding a token for this account
// (handoff §A.22). One card per device; the card itself is DeviceCard.tsx.
//
// Revoking the current device kills the bearer token this page is using, so
// that case clears the local session and goes to /login instead of reloading a
// list it can no longer read.

import { useState } from "react";
import { useNavigate } from "react-router-dom";
import * as authApi from "../../../api/auth";
import type { Device } from "../../../api/auth";
import { ApiError } from "../../../api/client";
import { useAuth } from "../../../auth/context";
import { clearSession } from "../../../auth/storage";
import { Button } from "../../../components/Button";
import { Modal } from "../../../components/Modal";
import { ResourceStates } from "../../../components/ResourceStates";
import { useToast } from "../../../components/Toast";
import { useResource } from "../../../lib/resource/react";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { DeviceCard, deviceLabel } from "./DeviceCard";

export function AccountDevices() {
  const { token, logout } = useAuth();
  const navigate = useNavigate();
  const { addToast } = useToast();
  const [busyId, setBusyId] = useState<string | null>(null);
  const [revokeTarget, setRevokeTarget] = useState<Device | null>(null);

  useSectionHead({
    sub: "Everything holding a token for your account, and what each one measured itself able to decode.",
  });

  const res = useResource<Device[]>({
    label: "your devices",
    fetch: async ({ token: t, signal }) => (await authApi.listDevices(t, signal)).devices,
    initialData: [],
  });
  const devices = res.data ?? [];

  async function handleRename(id: string, name: string) {
    if (!token) return;
    setBusyId(id);
    try {
      const { device: updated } = await authApi.updateDevice(token, id, { name });
      res.setData((list) => list.map((d) => (d.id === id ? updated : d)));
      addToast({ variant: "success", title: "Device renamed" });
    } catch (e) {
      addToast({
        variant: "danger",
        title: "Could not rename device",
        body: e instanceof ApiError ? e.message : undefined,
      });
    } finally {
      setBusyId(null);
    }
  }

  async function handleSetTrusted(id: string, trusted: boolean) {
    if (!token) return;
    setBusyId(id);
    try {
      const { device: updated } = await authApi.updateDevice(token, id, { trusted });
      res.setData((list) => list.map((d) => (d.id === id ? updated : d)));
    } catch (e) {
      addToast({
        variant: "danger",
        title: "Could not update device",
        body: e instanceof ApiError ? e.message : undefined,
      });
    } finally {
      setBusyId(null);
    }
  }

  async function handleRevoke(device: Device) {
    if (!token) return;
    setBusyId(device.id);
    try {
      await authApi.revokeDevice(token, device.id);
      setRevokeTarget(null);
      if (device.current) {
        addToast({
          variant: "success",
          title: "Device revoked",
          body: "You have been signed out.",
        });
        clearSession();
        await logout();
        navigate("/login", { replace: true });
        return;
      }
      addToast({ variant: "success", title: "Device revoked" });
      void res.refresh();
    } catch (e) {
      addToast({
        variant: "danger",
        title: "Could not revoke device",
        body: e instanceof ApiError ? e.message : undefined,
      });
    } finally {
      setBusyId(null);
    }
  }

  return (
    <div className="card sec-card">
      <ResourceStates
        loading={res.loading}
        error={res.errorMessage}
        isEmpty={devices.length === 0}
        empty="No device record yet. Capabilities are measured the first time you sign in."
      />

      {devices.length > 0 && (
        <div className="dev-grid">
          {devices.map((d) => (
            <DeviceCard
              key={d.id}
              device={d}
              busy={busyId === d.id}
              onRename={handleRename}
              onSetTrusted={handleSetTrusted}
              onRevoke={setRevokeTarget}
            />
          ))}
        </div>
      )}

      <p className="note mt5">
        Capabilities are measured at sign-in, not advertised by the device. Revoking signs
        that device out immediately and ends any session it is running.
      </p>

      {revokeTarget && (
        <Modal
          open
          onClose={() => setRevokeTarget(null)}
          title="Revoke device"
          footer={
            <>
              <Button variant="ghost" onClick={() => setRevokeTarget(null)}>
                Cancel
              </Button>
              <Button
                variant="danger"
                disabled={busyId === revokeTarget.id}
                onClick={() => void handleRevoke(revokeTarget)}
              >
                {busyId === revokeTarget.id ? "Revoking…" : "Revoke device"}
              </Button>
            </>
          }
        >
          <p className="sec">
            This signs <strong>{deviceLabel(revokeTarget)}</strong> out immediately and revokes
            its access.
            {revokeTarget.current && (
              <>
                {" "}
                This is <strong>this device</strong>, so you will be signed out right now.
              </>
            )}
            {revokeTarget.active_session_id && (
              <> It has a live streaming session, which will end.</>
            )}
          </p>
        </Modal>
      )}
    </div>
  );
}
