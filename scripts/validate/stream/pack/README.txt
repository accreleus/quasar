Quasar streaming test: what to do
=================================

What you need
  * Node 22 (https://nodejs.org) and Google Chrome, on this machine.
  * The stack's address, and a test user's e-mail and password. You were given them.
  * Internet for the first run only (it fetches one small library).

What to do
  1. Make a text file named signin.txt in this folder, with two lines:
         QUASAR_E2E_EMAIL=the e-mail you were given
         QUASAR_E2E_PASSWORD=the password you were given
  2. Open a terminal in this folder and run ONE command:
         macOS / Linux:   bash run.sh https://the-stack-address
         Windows:         run.cmd https://the-stack-address
     It checks this machine first and says plainly what is wrong, if anything. If the
     stack uses a self-made certificate, add   --insecure   after the address. The test
     streams for several minutes and opens a Chrome window: leave it alone until the
     command ends.
     Just to check this machine without streaming:  bash run.sh doctor https://the-stack-address
  3. When it ends it prints the path of one .zip file (in the "results" folder).

What to send back
  Send that one .zip file, whether the streams passed or not. It holds no password and
  no address. Do not send signin.txt.

macOS: if it says Chrome cannot reach the stack, open System Settings > Privacy &
Security > Local Network and allow Google Chrome, then run it again.
